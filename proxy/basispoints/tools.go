package basispoints

import (
	"container/list"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"
)

type tool struct {
	Name       string
	Namespace  string
	Kind       string
	Definition string
	Parameters object
	// RawField names the argument the raw-field transport fills verbatim;
	// empty when the tool is not eligible. RawAsList wraps the value in a
	// one-element array (the built-in shell tool's commands).
	RawField  string
	RawAsList bool
}

type replayEntry struct {
	key             string
	raw             []byte
	callFingerprint string
}

// ReplayCache retains native tool identities without mixing accounts or sessions.
// Both entry count and bytes are bounded because tool arguments can be large.
type ReplayCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
	bytes   int
	backing ReplayBacking
	// backingDownUntil pauses the backing after a failure so a slow or
	// unavailable store cannot stall request translation.
	backingDownUntil time.Time
	writerOnce       sync.Once
	writes           chan replayWrite
	pendingWrites    sync.WaitGroup
}

// ReplayBacking persists exact native tool items outside this process.
// Without it a restarted or different gateway instance can only rebuild
// approximate run_officejs items, and encrypted reasoning then treats earlier
// tool results as unrelated. Implementations must bound their own latency.
type ReplayBacking interface {
	Load(key string) ([]byte, bool, error)
	Store(key string, value []byte) error
}

type replayRecord struct {
	Raw  json.RawMessage `json:"raw"`
	Call string          `json:"call,omitempty"`
}

type replayWrite struct {
	key   string
	value []byte
}

const (
	replayBackingCooldown    = 30 * time.Second
	replayPrefetchBudget     = time.Second
	replayPrefetchWorkers    = 8
	replayWriteQueueCapacity = 256
)

// SetBacking attaches a shared store; nil keeps the cache process-local.
func (c *ReplayCache) SetBacking(backing ReplayBacking) {
	c.mu.Lock()
	c.backing = backing
	c.backingDownUntil = time.Time{}
	c.mu.Unlock()
}

// activeBacking returns the shared store, or nil when none is attached or it
// is cooling down after a failure.
func (c *ReplayCache) activeBacking() ReplayBacking {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.backing == nil || time.Now().Before(c.backingDownUntil) {
		return nil
	}
	return c.backing
}

// backingFailed pauses the shared store for replayBackingCooldown and logs
// only the first failure of each outage.
func (c *ReplayCache) backingFailed(err error) {
	c.mu.Lock()
	alreadyDown := time.Now().Before(c.backingDownUntil)
	c.backingDownUntil = time.Now().Add(replayBackingCooldown)
	c.mu.Unlock()
	if !alreadyDown {
		log.Printf("[excel-bps] replay store unavailable, using memory only for %s: %v", replayBackingCooldown, err)
	}
}

// put records an item in memory only. Rebuilt or derived items go here: they
// are reproducible from client history, and persisting them could overwrite
// the exact native item another instance stored for the same call.
func (c *ReplayCache) put(scope, id string, item object, clientCall ...object) {
	c.store(scope, id, item, false, clientCall...)
}

// putNative records an exact item returned by BPS and also persists it.
func (c *ReplayCache) putNative(scope, id string, item object, clientCall ...object) {
	c.store(scope, id, item, true, clientCall...)
}

// store inserts an item into the in-memory LRU and, when persist is set and
// the backing is healthy, queues it for the background writer. A full queue
// drops the write rather than blocking the response being translated.
func (c *ReplayCache) store(scope, id string, item object, persist bool, clientCall ...object) {
	if c == nil || id == "" {
		return
	}
	raw, err := json.Marshal(item)
	if err != nil || len(raw) > 1<<20 {
		return
	}
	var signature string
	if len(clientCall) == 1 {
		signature = historyCallFingerprint(clientCall[0])
	}
	key := scope + "\x00" + id
	c.mu.Lock()
	c.insertLocked(replayEntry{key: key, raw: raw, callFingerprint: signature})
	c.mu.Unlock()
	if !persist || c.activeBacking() == nil {
		return
	}
	record, err := json.Marshal(replayRecord{Raw: raw, Call: signature})
	if err != nil {
		return
	}
	c.writerOnce.Do(func() {
		c.writes = make(chan replayWrite, replayWriteQueueCapacity)
		go c.writeLoop()
	})
	c.pendingWrites.Add(1)
	select {
	case c.writes <- replayWrite{key: key, value: record}:
	default:
		// Never block a response on the shared store; this instance still
		// replays the item from memory.
		c.pendingWrites.Done()
	}
}

// writeLoop drains queued writes one at a time; a failed Store pauses the
// backing so the remaining queue is skipped until the cooldown ends.
func (c *ReplayCache) writeLoop() {
	for write := range c.writes {
		if backing := c.activeBacking(); backing != nil {
			if err := backing.Store(write.key, write.value); err != nil {
				c.backingFailed(err)
			}
		}
		c.pendingWrites.Done()
	}
}

func (c *ReplayCache) insertLocked(entry replayEntry) {
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	if old := c.entries[entry.key]; old != nil {
		// Only replayEntry values are inserted into this private list.
		previous, _ := old.Value.(replayEntry)
		c.bytes -= len(previous.raw)
		c.order.Remove(old)
	}
	c.entries[entry.key] = c.order.PushBack(entry)
	c.bytes += len(entry.raw)
	for len(c.entries) > 1024 || c.bytes > 16<<20 {
		old := c.order.Front()
		evicted, _ := old.Value.(replayEntry)
		delete(c.entries, evicted.key)
		c.bytes -= len(evicted.raw)
		c.order.Remove(old)
	}
}

// Prefetch loads call IDs missing from memory in one bounded concurrent pass
// before translation, so every later lookup is a memory read. Loads that miss
// the budget are treated as absent and the calls are rebuilt locally.
func (c *ReplayCache) Prefetch(scope string, ids []string) {
	backing := c.activeBacking()
	if backing == nil {
		return
	}
	seen := make(map[string]bool, len(ids))
	var missing []string
	c.mu.Lock()
	for _, id := range ids {
		key := scope + "\x00" + id
		if id == "" || seen[key] || c.entries[key] != nil {
			continue
		}
		seen[key] = true
		missing = append(missing, key)
	}
	c.mu.Unlock()
	if len(missing) == 0 {
		return
	}
	deadline := time.Now().Add(replayPrefetchBudget)
	keys := make(chan string)
	var workers sync.WaitGroup
	for range min(replayPrefetchWorkers, len(missing)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for key := range keys {
				c.hydrate(backing, key)
			}
		}()
	}
	for _, key := range missing {
		if time.Now().After(deadline) || c.activeBacking() == nil {
			break
		}
		keys <- key
	}
	close(keys)
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
		// Stragglers finish in the background and only warm the memory cache.
	}
}

// hydrate loads one key into memory unless a newer local entry (for example a
// rebuilt item) already exists, so a late prefetch never overwrites it.
func (c *ReplayCache) hydrate(backing ReplayBacking, key string) {
	raw, ok, err := backing.Load(key)
	if err != nil {
		c.backingFailed(err)
		return
	}
	var record replayRecord
	if !ok || json.Unmarshal(raw, &record) != nil || len(record.Raw) == 0 || len(record.Raw) > 1<<20 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[key] == nil {
		c.insertLocked(replayEntry{key: key, raw: record.Raw, callFingerprint: record.Call})
	}
}

func (c *ReplayCache) get(scope, id string) object {
	return c.getMatching(scope, id, "", false)
}

// A complete client call is stronger evidence than a reused call ID. Matching
// ignores wire-only item IDs/status and JSON object order, but retains the tool
// kind, namespace, argument values and exact custom input.
func historyCallFingerprint(item object) string {
	if isBuiltinCall(item) {
		return builtinCallFingerprint(item)
	}
	kind, id, name := text(item["type"]), text(item["call_id"]), text(item["name"])
	if id == "" || name == "" || strings.TrimSpace(id) != id || strings.TrimSpace(name) != name {
		return ""
	}
	namespace := ""
	if raw, exists := item["namespace"]; exists {
		var ok bool
		namespace, ok = raw.(string)
		if !ok || strings.TrimSpace(namespace) != namespace {
			return ""
		}
	}
	canonical := object{"type": kind, "call_id": id, "name": name, "namespace": namespace}
	switch kind {
	case "function_call":
		arguments := item["arguments"]
		if raw, ok := arguments.(string); ok {
			if decode([]byte(raw), &arguments) != nil {
				return ""
			}
		}
		if args, ok := arguments.(object); !ok || args == nil {
			return ""
		}
		canonical["arguments"] = arguments
	case "custom_tool_call":
		input, ok := item["input"].(string)
		if !ok {
			return ""
		}
		canonical["input"] = input
	default:
		return ""
	}
	return fingerprint(canonical)
}

func (c *ReplayCache) getForCall(scope, id string, clientCall object) object {
	signature := historyCallFingerprint(clientCall)
	if signature == "" {
		return nil
	}
	return c.getMatching(scope, id, signature, true)
}

func (c *ReplayCache) getMatching(scope, id, signature string, requireSignature bool) object {
	if c == nil || id == "" {
		return nil
	}
	c.mu.Lock()
	var cached replayEntry
	found := false
	if element := c.entries[scope+"\x00"+id]; element != nil {
		cached, found = element.Value.(replayEntry)
		c.order.MoveToBack(element)
	}
	c.mu.Unlock()
	if !found || (requireSignature && cached.callFingerprint != signature) {
		return nil
	}
	var item object
	if decode(cached.raw, &item) != nil {
		return nil
	}
	return item
}

func (b *Bridge) collectTools(value any, namespace string) ([]any, error) {
	var catalog []any
	items, _ := value.([]any)
	for _, raw := range items {
		item, ok := raw.(object)
		if !ok {
			return nil, fmt.Errorf("invalid Basispoints client tool")
		}
		kind, name := text(item["type"]), text(item["name"])
		if kind == "namespace" {
			if name == "" {
				return nil, fmt.Errorf("basispoints client namespaces require a name")
			}
			nestedNamespace := name
			if namespace != "" {
				nestedNamespace = namespace + "." + name
			}
			nested, err := b.collectTools(item["tools"], nestedNamespace)
			if err != nil {
				return nil, err
			}
			catalog = append(catalog, nested...)
			continue
		}
		if _, builtin := builtinClientTools[kind]; builtin {
			// Registered after every declared tool, so a client tool with the
			// same name always wins regardless of declaration order.
			if !slices.Contains(b.builtins, kind) {
				b.builtins = append(b.builtins, kind)
			}
			continue
		}
		if kind != "function" && kind != "custom" {
			// Server-hosted and unknown tool kinds (web_search, file_search, ...)
			// cannot be relayed as client calls. Omit them with a model-visible
			// warning instead of failing the request.
			if kind == "" {
				kind = "unknown"
			}
			if b.unsupportedTools == nil {
				b.unsupportedTools = make(map[string]bool)
			}
			b.unsupportedTools[kind] = true
			continue
		}
		if name == "" {
			return nil, fmt.Errorf("basispoints client tools require a name")
		}
		key := name
		if namespace != "" {
			key = namespace + "." + name
		}
		entry := object{"type": kind, "name": key}
		for _, field := range []string{"description", "format", "parameters"} {
			if v, exists := item[field]; exists {
				entry[field] = v
			}
		}
		if kind == "function" && entry["parameters"] == nil {
			entry["parameters"] = item["inputSchema"]
			if entry["parameters"] == nil {
				entry["parameters"] = item["input_schema"]
			}
		}
		definition := fingerprint(item)
		if previous, exists := b.tools[key]; exists {
			if previous.Definition != definition || previous.Namespace != namespace || previous.Name != name {
				return nil, fmt.Errorf("conflicting duplicate Basispoints client tool %q", key)
			}
			continue
		}
		parameters, _ := entry["parameters"].(object)
		info := tool{Name: name, Namespace: namespace, Kind: kind, Definition: definition, Parameters: parameters}
		if kind == "function" {
			info.RawField = rawFieldFor(parameters)
			if info.RawField != "" {
				entry[catalogRawFieldKey] = info.RawField
			}
		}
		b.tools[key] = info
		catalog = append(catalog, entry)
	}
	return catalog, nil
}

// rebuildNativeHistoryCall uses only the complete call supplied by the client.
// It does not execute a tool or require that an old tool remain in today's
// catalog. Cached native items remain authoritative when available.
func rebuildNativeHistoryCall(item object) (object, error) {
	if isBuiltinCall(item) {
		name, args, err := builtinEnvelope(item)
		if err != nil {
			return nil, err
		}
		return transportHistoryCall(item, object{"name": name, "arguments": args})
	}
	id, name := text(item["call_id"]), text(item["name"])
	if id == "" || strings.TrimSpace(id) != id || name == "" || strings.TrimSpace(name) != name {
		return nil, fmt.Errorf("basispoints history recovery requires a complete tool call with nonempty call_id and name")
	}
	if value, exists := item["namespace"]; exists {
		namespace, ok := value.(string)
		if !ok || strings.TrimSpace(namespace) != namespace {
			return nil, fmt.Errorf("basispoints history tool namespace must be a string")
		}
		if namespace != "" {
			name = namespace + "." + name
		}
	}
	envelope := object{"name": name}
	switch text(item["type"]) {
	case "function_call":
		arguments := item["arguments"]
		if encoded, ok := arguments.(string); ok {
			if decode([]byte(encoded), &arguments) != nil {
				return nil, fmt.Errorf("basispoints history function arguments must contain one valid JSON object")
			}
		}
		if args, ok := arguments.(object); !ok || args == nil {
			return nil, fmt.Errorf("basispoints history function arguments must be a JSON object")
		}
		envelope["arguments"] = arguments
	case "custom_tool_call":
		input, ok := item["input"].(string)
		if !ok {
			return nil, fmt.Errorf("basispoints history custom tool input must be a string")
		}
		envelope["input"] = input
	default:
		return nil, fmt.Errorf("basispoints history recovery requires a function or custom tool call")
	}
	return transportHistoryCall(item, envelope)
}

// transportHistoryCall wraps a recovered envelope in the run_officejs item BPS
// knows, keeping the client's call_id so the following result still pairs.
func transportHistoryCall(item object, envelope object) (object, error) {
	id := text(item["call_id"])
	if id == "" || strings.TrimSpace(id) != id {
		return nil, fmt.Errorf("basispoints history recovery requires a complete tool call with nonempty call_id")
	}
	code, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("basispoints history tool arguments cannot be serialized")
	}
	arguments, err := json.Marshal(object{
		"code": string(code), "summary": "Replay a previously requested client tool",
		"extended_summary": "The supplied client history contains this tool call; consume its recorded result without repeating it.",
		"destructive":      false, "references": []any{},
	})
	if err != nil {
		return nil, fmt.Errorf("basispoints history transport cannot be serialized")
	}
	itemID := text(item["id"])
	if !strings.HasPrefix(itemID, "fc_") || len(itemID) > 64 {
		itemID = "fc_" + fingerprint(id)
	}
	return object{
		"type": "function_call", "id": itemID, "call_id": id, "name": "run_officejs",
		"arguments": string(arguments), "status": "completed",
	}, nil
}

// emptyToolOutput replaces a blank tool result, which the model reads as a
// failed call and retries.
const emptyToolOutput = "(tool call succeeded with no output)"

// nativePlanResult is what the Excel update_plan executor returns. Codex's own
// plan tool answers "Plan updated", which leaves the native plan step
// unresolved upstream and makes the model plan again.
const nativePlanResult = `{"status":"ok"}`

func isNativePlan(item object) bool {
	name := text(item["name"])
	return text(item["type"]) == "function_call" && (name == "update_plan" || name == "functions.update_plan")
}

func (b *Bridge) translateHistory(input []any) ([]any, error) {
	result := make([]any, 0, len(input))
	seenCalls := make(map[string]bool)
	nativePlans := make(map[string]bool)
	// callIDs maps a client call item's id to its call_id: the Responses
	// local_shell_call_output names its call by id rather than call_id.
	callIDs := make(map[string]string)
	var trigger any
	for _, raw := range input {
		item, ok := raw.(object)
		if !ok {
			return nil, fmt.Errorf("invalid Basispoints input item")
		}
		switch text(item["type"]) {
		case "additional_tools":
			continue
		case "item_reference":
			// References point at stored responses, which store=false never
			// creates. Skip them like the add-in instead of failing the turn;
			// Prepare tells the model that part of the history is missing.
			b.skippedReferences++
			continue
		case "compaction_trigger":
			trigger = item
			continue
		case "reasoning":
			if encrypted := text(item["encrypted_content"]); encrypted != "" {
				result = append(result, object{"type": "reasoning", "summary": []any{}, "encrypted_content": encrypted})
			}
			continue
		case "function_call", "custom_tool_call", "local_shell_call", "shell_call", "apply_patch_call":
			id := text(item["call_id"])
			if itemID := text(item["id"]); itemID != "" && id != "" {
				callIDs[itemID] = id
			}
			if native := b.replay.getForCall(b.scope, id, item); native != nil {
				item = native
			} else {
				native, err := rebuildNativeHistoryCall(item)
				if err != nil {
					return nil, err
				}
				b.replay.put(b.scope, id, native, item)
				item = native
			}
			seenCalls[id] = true
			nativePlans[id] = isNativePlan(item)
		case "function_call_output", "custom_tool_call_output", "local_shell_call_output", "shell_call_output", "apply_patch_call_output":
			id := text(item["call_id"])
			if _, builtin := builtinKindForCall(text(item["type"])); builtin {
				if id == "" {
					// Spec-shaped local_shell_call_output carries only id, which
					// names the call item (or, in some clients, its call_id).
					ref := text(item["id"])
					if id = callIDs[ref]; id == "" {
						id = ref
					}
				}
				// BPS only knows run_officejs results; keep the pairing call_id
				// and render the structured result as text. The client's id
				// may name the call item, so the result gets its own fc_ id.
				item = object{"type": "function_call_output", "call_id": id, "output": builtinOutputText(item)}
			}
			if !seenCalls[id] {
				native := b.replay.get(b.scope, id)
				if native == nil {
					return nil, fmt.Errorf("basispoints original tool item is unavailable for this tool result; start a new conversation")
				}
				result = append(result, native)
				seenCalls[id] = true
				nativePlans[id] = isNativePlan(native)
			}
			item["type"] = "function_call_output"
			if nativePlans[id] {
				item["output"] = nativePlanResult
			} else if output, ok := item["output"].(string); item["output"] == nil || ok && strings.TrimSpace(output) == "" {
				item["output"] = emptyToolOutput
			}
			if err := validateHistoryContent(item["output"]); err != nil {
				return nil, err
			}
			// Codex custom results carry ctco_ IDs. After lowering to a function
			// result, BPS requires an fc_ item ID even when the client supplied one.
			itemID := text(item["id"])
			if itemID == "" {
				itemID = "fc_" + id
			}
			if !strings.HasPrefix(itemID, "fc_") || len(itemID) > 64 {
				itemID = "fc_" + fingerprint(itemID)
			}
			item["id"] = itemID
		case "configuration_update":
			return nil, fmt.Errorf("basispoints does not support configuration_update; start a new request with the desired effort")
		}
		if err := validateHistoryContent(item["content"]); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if trigger != nil {
		result = append(result, trigger)
	}
	return result, nil
}

func validateHistoryContent(value any) error {
	content, _ := value.([]any)
	for _, rawPart := range content {
		part, _ := rawPart.(object)
		switch text(part["type"]) {
		case "input_text", "output_text", "text", "refusal":
		case "input_image":
			if err := validateImage(part); err != nil {
				return err
			}
		default:
			return fmt.Errorf("basispoints supports text and HTTPS input_image content only")
		}
	}
	return nil
}

func isTool(item object) bool {
	return text(item["type"]) == "function_call" || text(item["type"]) == "custom_tool_call"
}

// translateCall accepts only the declared relay transport and a caller-declared tool.
// It does not evaluate code or dispatch any Excel operation.
func (b *Bridge) translateCall(native object) (object, error) {
	name := text(native["name"])
	if text(native["type"]) == "function_call" && (name == "update_plan" || name == "functions.update_plan") {
		return b.translateNativePlan(native)
	}
	if name != "run_officejs" && name != "functions.run_officejs" {
		return b.translateDirectCatalogCall(native)
	}
	var arguments object
	if value, ok := native["arguments"].(object); ok {
		arguments = value
	} else if err := decode([]byte(text(native["arguments"])), &arguments); err != nil {
		return nil, fmt.Errorf("basispoints returned invalid tool transport arguments")
	}
	if arguments == nil {
		return nil, fmt.Errorf("basispoints returned empty tool transport arguments")
	}
	envelope, raw, err := b.rawFieldEnvelope(arguments)
	if err != nil {
		return nil, err
	}
	marked := false
	if !raw {
		envelope, marked, err = customTransportEnvelope(arguments)
		if !marked && err == nil {
			envelope, err = decodeTransportEnvelope(arguments["code"])
		}
		if err != nil {
			return nil, err
		}
	}
	toolName, err := envelopeName(envelope)
	if err != nil {
		return nil, err
	}
	info, allowed := b.tools[toolName]
	if !allowed {
		return nil, fmt.Errorf("basispoints returned a tool outside the client's catalog")
	}
	result, err := b.finishClientToolCall(native, info, envelope, marked)
	if err != nil {
		return nil, err
	}
	// run_officejs is a real BPS-native tool, so its item replays upstream verbatim.
	b.replay.putNative(b.scope, text(native["call_id"]), native, result)
	return result, nil
}

// translateDirectCatalogCall recovers a native tool call the model addressed by the
// client tool's own name instead of through the run_officejs transport. Some turns
// skip the wrapper and call the catalog tool directly; the reference plugins accept
// this rather than failing the whole response. It relays the client's declared tool
// call to the client unchanged and never executes any code. Only exact catalog names
// (optionally carrying a host "functions." display prefix) are accepted; any other
// native tool remains an unsupported-native-tool error.
func (b *Bridge) translateDirectCatalogCall(native object) (object, error) {
	name := text(native["name"])
	info, ok := b.tools[name]
	if !ok {
		if trimmed := strings.TrimPrefix(name, "functions."); trimmed != name {
			info, ok = b.tools[trimmed]
		}
	}
	if !ok {
		return nil, fmt.Errorf("basispoints returned an unsupported native tool; no tool was executed")
	}
	kind := text(native["type"])
	var envelope object
	switch info.Kind {
	case "function":
		if kind != "function_call" {
			return nil, fmt.Errorf("basispoints returned client function tool %q as a %q; no tool was executed", info.Name, kind)
		}
		envelope = object{"name": info.Name, "arguments": native["arguments"]}
	case "local_shell", "shell", "apply_patch":
		if kind != "function_call" {
			return nil, fmt.Errorf("basispoints returned built-in tool %q as a %q; no tool was executed", info.Name, kind)
		}
		envelope = object{"name": info.Name, "arguments": native["arguments"]}
	case "custom":
		if kind != "custom_tool_call" {
			return nil, fmt.Errorf("basispoints returned client custom tool %q as a %q; no tool was executed", info.Name, kind)
		}
		input, ok := native["input"].(string)
		if !ok {
			return nil, fmt.Errorf("basispoints direct custom tool input must be a string")
		}
		envelope = object{"name": info.Name, "input": input}
	default:
		return nil, fmt.Errorf("basispoints returned an unsupported native tool; no tool was executed")
	}
	result, err := b.finishClientToolCall(native, info, envelope, false)
	if err != nil {
		return nil, err
	}
	// The model bypassed run_officejs, so the bare native name is not a BPS tool.
	// Cache a transport-wrapped replay so the next turn presents a BPS-known
	// run_officejs item, matching how absent history is rebuilt.
	wrapped, err := rebuildNativeHistoryCall(result)
	if err != nil {
		return nil, err
	}
	b.replay.put(b.scope, text(native["call_id"]), wrapped, result)
	return result, nil
}

// finishClientToolCall builds the client-facing tool item from a resolved catalog
// tool and its envelope. It performs no caching and executes nothing; callers decide
// how the call replays upstream.
func (b *Bridge) finishClientToolCall(native object, info tool, envelope object, marked bool) (object, error) {
	if marked && info.Kind != "custom" {
		return nil, fmt.Errorf("basispoints raw transport requires a declared custom tool")
	}
	id := text(native["call_id"])
	if id == "" {
		return nil, fmt.Errorf("basispoints tool call is missing call_id")
	}
	if _, builtin := builtinClientTools[info.Kind]; builtin {
		args, err := envelopeArguments(envelope)
		if err != nil {
			return nil, err
		}
		if raw, ok := args.(string); ok {
			if decode([]byte(raw), &args) != nil {
				return nil, fmt.Errorf("basispoints %s arguments are invalid JSON", info.Kind)
			}
		}
		fields, ok := args.(object)
		if !ok {
			return nil, fmt.Errorf("basispoints %s arguments must be an object", info.Kind)
		}
		return builtinCallItem(info.Kind, id, fields)
	}
	itemID := text(native["id"])
	if itemID == "" {
		itemID = "fc_" + fingerprint(id)
	}
	result := object{"type": info.Kind + "_call", "id": itemID, "call_id": id, "name": info.Name, "status": "completed"}
	if info.Namespace != "" {
		result["namespace"] = info.Namespace
	}
	if info.Kind == "custom" {
		value, hasInput := envelope["input"]
		if alias, hasAlias := envelope["args"]; hasAlias {
			if hasInput {
				return nil, fmt.Errorf("basispoints custom tool envelope contains conflicting input fields")
			}
			value = alias
		}
		if _, exists := envelope["arguments"]; exists {
			return nil, fmt.Errorf("basispoints custom tools require input text, not arguments")
		}
		input, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("basispoints custom tool input must be a string")
		}
		result["type"] = "custom_tool_call"
		result["id"] = "ctc_" + fingerprint(id)
		result["input"] = input
	} else {
		args, err := envelopeArguments(envelope)
		if err != nil {
			return nil, err
		}
		if raw, ok := args.(string); ok {
			if decode([]byte(raw), &args) != nil {
				return nil, fmt.Errorf("basispoints function arguments are invalid JSON")
			}
		}
		if _, ok := args.(object); !ok {
			return nil, fmt.Errorf("basispoints function arguments must be an object")
		}
		encoded, _ := json.Marshal(args)
		result["arguments"] = string(encoded)
	}
	return result, nil
}

// translateResponse converts the native tool calls of a completed response.
// A call that cannot be relayed (an Excel-native tool, OfficeJS in the
// transport, a malformed envelope) is dropped rather than failing the other
// calls and the text of the same response; nothing is executed either way.
func (b *Bridge) translateResponse(response object) error {
	if response == nil {
		return nil
	}
	output, _ := response["output"].([]any)
	kept := make([]any, 0, len(output))
	calls, dropped := 0, 0
	answered := false
	for _, raw := range output {
		item, _ := raw.(object)
		if !isTool(item) {
			if text(item["type"]) == "reasoning" {
				normalizeReasoningForClient(item)
			}
			answered = answered || hasAssistantText(item)
			kept = append(kept, raw)
			continue
		}
		if calls > 0 && !b.Parallel {
			log.Printf("[excel-bps] dropped an extra tool call because the client disabled parallel_tool_calls")
			continue
		}
		translated, err := b.translateCall(item)
		if err != nil {
			log.Printf("[excel-bps] dropped an untranslatable tool call: %v", err)
			dropped++
			continue
		}
		kept = append(kept, translated)
		calls++
	}
	if dropped > 0 && calls == 0 && !answered {
		// Nothing usable is left; fail so the client retries instead of
		// ending the turn on an empty "successful" response.
		return fmt.Errorf("basispoints response contained only tool calls that could not be relayed")
	}
	response["output"] = kept
	response["reasoning"] = object{"effort": b.Effort}
	response["parallel_tool_calls"] = b.Parallel
	return nil
}

func hasAssistantText(item object) bool {
	// Commentary precedes tool calls; it does not end a turn on its own, the
	// same rule the cut-off completion applies.
	if text(item["type"]) != "message" || text(item["phase"]) == "commentary" {
		return false
	}
	content, _ := item["content"].([]any)
	for _, raw := range content {
		part, _ := raw.(object)
		if strings.TrimSpace(text(part["text"])) != "" {
			return true
		}
	}
	return false
}

// reasoningHeader matches the bold title Codex expects on a reasoning summary.
const reasoningHeader = "**Thinking**\n\n"

// normalizeReasoningForClient fills a reasoning item's summary so Codex shows
// it; Codex renders only summary text and drops items whose summary is empty.
// Upstream replay is unaffected: history keeps only encrypted_content.
func normalizeReasoningForClient(item object) {
	if item == nil || text(item["type"]) != "reasoning" {
		return
	}
	joined := func(value any) string {
		parts, _ := value.([]any)
		var out strings.Builder
		for _, raw := range parts {
			switch part := raw.(type) {
			case string:
				out.WriteString(part)
			case object:
				out.WriteString(text(part["text"]))
			}
		}
		return out.String()
	}
	body := joined(item["summary"])
	if body == "" {
		body = joined(item["content"])
	}
	if body == "" && text(item["encrypted_content"]) != "" {
		body = "*Thinking process completed.*"
	}
	if strings.TrimSpace(body) == "" {
		return
	}
	if trimmed := strings.TrimLeft(body, " \t\r\n"); !strings.HasPrefix(trimmed, "**") && !strings.HasPrefix(trimmed, "#") {
		body = reasoningHeader + body
	}
	item["summary"] = []any{object{"type": "summary_text", "text": body}}
	item["content"] = []any{object{"type": "reasoning_text", "text": body}}
}

func isToolEvent(kind string) bool {
	return strings.HasPrefix(kind, "response.function_call_arguments.") || strings.HasPrefix(kind, "response.custom_tool_call_input.")
}
