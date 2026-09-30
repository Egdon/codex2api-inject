package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// Basispoints reports some rejections after HTTP 200: its token rate limit
// arrives as response.created, response.in_progress, then an error event and
// response.failed, with no output. Seen before any output, such a rejection
// is handled like the equivalent HTTP status, so the route cools down and the
// request can still fall back to native Codex. Inspection stops at the first
// event that is not a lifecycle announcement, or after a short window so a
// slow first output never holds the stream back, and everything read is
// replayed to the bridge unchanged.
const excelBPSPreOutputWindow = 3 * time.Second

// inspectExcelBPSStart returns the pre-output rejection of a successful
// Basispoints response and closes its body, or returns nil after replacing
// response.Body with a reader that replays what was inspected.
func inspectExcelBPSStart(ctx context.Context, response *http.Response, window time.Duration) *excelBPSHTTPError {
	body := newExcelBPSReplayBody(response.Body)
	response.Body = body
	timer := time.NewTimer(window)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-body.lines:
			if !ok {
				return nil
			}
			body.queue = append(body.queue, line)
			if line.err != nil {
				return nil
			}
			data, isData := strings.CutPrefix(strings.TrimRight(string(line.data), "\r\n"), "data:")
			if !isData {
				continue
			}
			event := []byte(strings.TrimSpace(data))
			switch gjson.GetBytes(event, "type").String() {
			case "response.created", "response.in_progress":
				continue
			case "error", "response.failed":
				if rejected := excelBPSPreOutputFailure(event); rejected != nil {
					_ = body.Close()
					return rejected
				}
			}
			return nil
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// excelBPSPreOutputFailure maps a terminal event to the HTTP status it stands
// for. Only rejections with a known retry meaning are mapped; anything else
// reaches the client as before.
func excelBPSPreOutputFailure(event []byte) *excelBPSHTTPError {
	failure := gjson.GetBytes(event, "error")
	if !failure.Exists() {
		failure = gjson.GetBytes(event, "response.error")
	}
	code := strings.ToLower(strings.TrimSpace(failure.Get("code").String()))
	kind := strings.ToLower(strings.TrimSpace(failure.Get("type").String()))
	switch {
	case strings.Contains(code, "rate_limit") || strings.Contains(kind, "rate_limit"):
		return &excelBPSHTTPError{status: http.StatusTooManyRequests, code: "rate_limit_exceeded"}
	case code == "server_is_overloaded":
		return &excelBPSHTTPError{status: http.StatusServiceUnavailable, code: code}
	}
	return nil
}

type excelBPSReplayLine struct {
	data []byte
	err  error
}

// excelBPSReplayBody pumps the upstream body line by line, so inspection can
// give up at a deadline without abandoning a blocked read, then serves the
// inspected lines followed by the rest of the stream.
type excelBPSReplayBody struct {
	lines   chan excelBPSReplayLine
	queue   []excelBPSReplayLine
	pending []byte
	err     error
	stop    chan struct{}
	once    sync.Once
	closer  io.Closer
}

func newExcelBPSReplayBody(upstream io.ReadCloser) *excelBPSReplayBody {
	body := &excelBPSReplayBody{lines: make(chan excelBPSReplayLine), stop: make(chan struct{}), closer: upstream}
	go func() {
		defer close(body.lines)
		reader := bufio.NewReaderSize(upstream, 64<<10)
		for {
			line, err := reader.ReadBytes('\n')
			select {
			case body.lines <- excelBPSReplayLine{data: line, err: err}:
			case <-body.stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return body
}

func (b *excelBPSReplayBody) Read(p []byte) (int, error) {
	for len(b.pending) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		var line excelBPSReplayLine
		if len(b.queue) > 0 {
			line, b.queue = b.queue[0], b.queue[1:]
		} else {
			select {
			case next, ok := <-b.lines:
				if !ok {
					return 0, http.ErrBodyReadAfterClose
				}
				line = next
			case <-b.stop:
				return 0, http.ErrBodyReadAfterClose
			}
		}
		b.pending, b.err = line.data, line.err
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *excelBPSReplayBody) Close() error {
	var err error
	b.once.Do(func() {
		close(b.stop)
		err = b.closer.Close()
	})
	return err
}
