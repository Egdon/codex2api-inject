package basispoints

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type protocolError struct{ error }

type streamBody struct {
	*io.PipeReader
	upstream io.ReadCloser
	once     sync.Once
	err      error
}

func (b *streamBody) closeUpstream() error {
	b.once.Do(func() { b.err = b.upstream.Close() })
	return b.err
}

func (b *streamBody) Close() error {
	readerErr := b.PipeReader.Close()
	return errors.Join(readerErr, b.closeUpstream())
}

// Stream keeps text incremental while withholding native tool events until validated.
// Closing the downstream body interrupts an upstream read or a blocked pipe write.
func (b *Bridge) Stream(upstream io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	body := &streamBody{PipeReader: reader, upstream: upstream}
	go func() {
		err := b.transform(upstream, writer)
		_ = body.closeUpstream()
		_ = writer.CloseWithError(err)
	}()
	return body
}

// defaultKeepalive is how long the upstream may stay silent before the bridge
// repeats response.in_progress. Codex treats a stream idle for five minutes as
// broken and retries the whole turn, which long reasoning can otherwise hit.
const defaultKeepalive = 15 * time.Second

type upstreamEvent struct {
	event string
	data  []byte
}

type finishedItem struct {
	index int
	item  object
}

func (b *Bridge) transform(reader io.Reader, writer io.Writer) error {
	sequence := 0
	terminal := false
	lastWrite := time.Now()
	var writeErr error
	emitted := make(map[string]bool)
	pendingTools := make(map[string]bool)
	// started, itemsAdded and finished describe the response so far, for
	// keepalive frames and for completing a stream cut off after its last item.
	var started object
	itemsAdded := 0
	var finished []finishedItem
	emit := func(kind string, payload object) error {
		payload["type"] = kind
		payload["sequence_number"] = sequence
		sequence++
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", kind, raw); err != nil {
			writeErr = err
			return err
		}
		lastWrite = time.Now()
		return nil
	}
	emitTool := func(item object, index any) error {
		id := text(item["id"])
		if emitted[id] {
			return nil
		}
		emitted[id] = true
		if isBuiltinCall(item) {
			// Built-in calls carry a structured action or operation and have
			// no argument delta events; the finished item is the whole call.
			added := make(object, len(item))
			for k, v := range item {
				added[k] = v
			}
			added["status"] = "in_progress"
			if err := emit("response.output_item.added", object{"output_index": index, "item": added}); err != nil {
				return err
			}
			return emit("response.output_item.done", object{"output_index": index, "item": item})
		}
		field, prefix := "arguments", "response.function_call_arguments"
		if text(item["type"]) == "custom_tool_call" {
			field, prefix = "input", "response.custom_tool_call_input"
		}
		added := make(object, len(item))
		for k, v := range item {
			added[k] = v
		}
		added[field], added["status"] = "", "in_progress"
		if err := emit("response.output_item.added", object{"output_index": index, "item": added}); err != nil {
			return err
		}
		if err := emit(prefix+".delta", object{"output_index": index, "item_id": id, "delta": item[field]}); err != nil {
			return err
		}
		if err := emit(prefix+".done", object{"output_index": index, "item_id": id, field: item[field]}); err != nil {
			return err
		}
		return emit("response.output_item.done", object{"output_index": index, "item": item})
	}
	process := func(event string, data []byte) error {
		if string(data) == "[DONE]" {
			return nil
		}
		var payload object
		if decode(data, &payload) != nil || payload == nil {
			return fmt.Errorf("invalid Basispoints SSE event")
		}
		kind := text(payload["type"])
		if kind == "" {
			kind = event
		}
		if kind == "response.completed" {
			if response, ok := payload["response"].(object); ok {
				switch strings.ToLower(strings.TrimSpace(text(response["status"]))) {
				case "failed":
					sanitizeTerminalError(payload, "response.failed")
				case "incomplete":
					sanitizeTerminalError(payload, "response.incomplete")
				}
			}
		}
		if kind == "response.failed" || kind == "response.incomplete" || kind == "error" {
			sanitizeTerminalError(payload, kind)
		}
		if isToolEvent(kind) {
			return nil
		}
		item, _ := payload["item"].(object)
		switch kind {
		case "response.output_item.added":
			itemsAdded++
		case "response.output_item.done":
			index := len(finished)
			if number, ok := payload["output_index"].(json.Number); ok {
				if value, err := number.Int64(); err == nil {
					index = int(value)
				}
			}
			if item != nil {
				finished = append(finished, finishedItem{index: index, item: item})
			}
		}
		if kind == "response.output_item.added" && isTool(item) {
			return nil
		}
		if kind == "response.output_item.done" && isTool(item) {
			// Only the terminal response contains the authoritative native item.
			// Text keeps streaming; tool calls wait until the whole response validates.
			if len(pendingTools) >= 1024 {
				return fmt.Errorf("basispoints response contains too many tool items")
			}
			pendingTools[text(item["call_id"])+"\x00"+text(item["id"])] = true
			return nil
		}
		if text(item["type"]) == "reasoning" {
			normalizeReasoningForClient(item)
		}
		if response, ok := payload["response"].(object); ok {
			if kind == "response.completed" {
				output, _ := response["output"].([]any)
				for _, raw := range output {
					item, _ := raw.(object)
					if isTool(item) {
						delete(pendingTools, text(item["call_id"])+"\x00"+text(item["id"]))
					}
				}
				if len(pendingTools) != 0 {
					return fmt.Errorf("basispoints completed response omitted an original tool item")
				}
				if err := b.translateResponse(response); err != nil {
					return err
				}
				output, _ = response["output"].([]any)
				for i, raw := range output {
					item, _ := raw.(object)
					if isClientCall(item) {
						if err := emitTool(item, i); err != nil {
							return err
						}
					}
				}
			} else {
				// Never expose native or incomplete tool arguments to the client.
				output, _ := response["output"].([]any)
				filtered := make([]any, 0, len(output))
				for _, raw := range output {
					item, _ := raw.(object)
					if !isTool(item) {
						filtered = append(filtered, raw)
					}
				}
				response["output"] = filtered
				response["reasoning"] = object{"effort": b.Effort}
				if kind == "response.created" || kind == "response.in_progress" {
					started = response
				}
			}
		}
		terminal = kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed" || kind == "error"
		return emit(kind, payload)
	}
	// completion rebuilds response.completed when the upstream closed after
	// finishing every item it started and the last one ends a turn: a final
	// answer message or a tool call. Anything else (reasoning, commentary
	// before tool calls) means the stream was cut mid-turn, which is left to
	// the client's retry instead of being reported as a finished answer.
	completion := func() []byte {
		if len(finished) == 0 || len(finished) < itemsAdded {
			return nil
		}
		sort.SliceStable(finished, func(i, j int) bool { return finished[i].index < finished[j].index })
		output := make([]any, 0, len(finished))
		for _, entry := range finished {
			output = append(output, entry.item)
		}
		last := finished[len(finished)-1].item
		endsTurn := isTool(last) || text(last["type"]) == "message" && text(last["phase"]) != "commentary"
		if !endsTurn {
			return nil
		}
		response := make(object, len(started)+2)
		for k, v := range started {
			response[k] = v
		}
		response["status"], response["output"] = "completed", output
		raw, err := json.Marshal(object{"type": "response.completed", "response": response})
		if err != nil {
			return nil
		}
		return raw
	}
	fail := func(err error) error {
		if writeErr != nil {
			return writeErr
		}
		log.Printf("[excel-bps] Basispoints response could not be translated: %v", err)
		return emit("response.failed", object{"response": object{
			"status": "failed", "output": []any{},
			"error": object{"code": "basispoints_protocol_error", "message": "Basispoints response could not be translated"},
		}})
	}

	events := make(chan upstreamEvent)
	readDone := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		readDone <- readEvents(reader, func(event string, data []byte) error {
			select {
			case events <- upstreamEvent{event: event, data: data}:
				return nil
			case <-stop:
				return io.EOF
			}
		})
	}()
	interval := b.keepalive
	if interval <= 0 {
		interval = defaultKeepalive
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case upstream := <-events:
			if err := process(upstream.event, upstream.data); err != nil {
				return fail(err)
			}
			if terminal {
				return nil
			}
		case err := <-readDone:
			var invalid protocolError
			if errors.As(err, &invalid) {
				return fail(invalid.error)
			}
			if closedLocally(err) {
				// The client went away and the body was closed on our side;
				// there is nobody to complete the response for.
				return err
			}
			if raw := completion(); raw != nil {
				reason := "closed"
				if err != nil {
					reason = err.Error()
				}
				log.Printf("[excel-bps] upstream stream ended before response.completed (%s); completing it from %d finished items", reason, len(finished))
				b.synthesized.Store(true)
				if perr := process("response.completed", raw); perr != nil {
					return fail(perr)
				}
				return nil
			}
			if err != nil {
				return err
			}
			return io.ErrUnexpectedEOF
		case <-ticker.C:
			if started != nil && time.Since(lastWrite) >= interval {
				if err := emit("response.in_progress", object{"response": started}); err != nil {
					return err
				}
			}
		}
	}
}

// TerminalErrorShape describes a provider error for operator logs using only
// its enum-like code and type fields. Messages are never logged because they
// can echo request content, connector arguments, or credential metadata.
func TerminalErrorShape(payload object) string {
	var source object
	if response, ok := payload["response"].(object); ok {
		source, _ = response["error"].(object)
	}
	if source == nil {
		source, _ = payload["error"].(object)
	}
	if source == nil {
		source = payload
	}
	return fmt.Sprintf("code=%q type=%q", truncateRunes(text(source["code"]), 64), truncateRunes(text(source["type"]), 64))
}

func closedLocally(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, http.ErrBodyReadAfterClose) || errors.Is(err, io.ErrClosedPipe)
}

// sanitizeTerminalError removes provider error text before a failed BPS event
// crosses the gateway boundary. Provider errors can echo request content,
// connector arguments, or credential metadata.
func sanitizeTerminalError(payload object, kind string) {
	log.Printf("[excel-bps] upstream %s: %s", kind, TerminalErrorShape(payload))
	message := "Basispoints upstream returned an unsuccessful response"
	code := "basispoints_upstream_error"
	if kind == "response.incomplete" {
		message = "Basispoints upstream ended the response before completion"
		code = "basispoints_incomplete"
	}
	if response, ok := payload["response"].(object); ok {
		delete(response, "status_details")
		response["error"] = object{"code": code, "message": message}
		delete(payload, "message")
		delete(payload, "code")
		delete(payload, "param")
		return
	}
	payload["error"] = object{"code": code, "message": message}
	delete(payload, "message")
	delete(payload, "code")
	delete(payload, "param")
}

func readEvents(reader io.Reader, consume func(string, []byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	var data strings.Builder
	event := ""
	flush := func() error {
		if data.Len() == 0 {
			event = ""
			return nil
		}
		err := consume(event, []byte(strings.TrimSuffix(data.String(), "\n")))
		data.Reset()
		event = ""
		return err
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
		} else if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			_, _ = data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			_ = data.WriteByte('\n')
			if data.Len() > 16<<20 {
				return protocolError{fmt.Errorf("basispoints SSE event exceeds 16 MiB")}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return protocolError{fmt.Errorf("basispoints SSE line exceeds 16 MiB")}
		}
		return err
	}
	return flush()
}
