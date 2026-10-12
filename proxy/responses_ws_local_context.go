package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const nativeWSCachePrefix = "native-ws:"
const nativeWSCommitMaxEntries = 2000
const nativeWSCommitWait = 5 * time.Second

var nativeWSCommits = struct {
	sync.Mutex
	pending map[string]chan struct{}
}{pending: make(map[string]chan struct{})}

func nativeWSCacheOwner(owner string, identity requestSessionIdentity) string {
	if identity.explicitUpstreamID == "" && !identity.hasDownstreamAffinity {
		return nativeWSCachePrefix + owner
	}
	hash := sha256.Sum256([]byte(identity.affinityID))
	return nativeWSCachePrefix + owner + ":" + hex.EncodeToString(hash[:])
}

type nativeWSTurnScope struct {
	body          []byte
	identity      requestSessionIdentity
	databaseScope string
}

func nativeWSTurnCacheOwner(c *gin.Context, owner string, scope nativeWSTurnScope) string {
	keyIdentity := deterministicPromptCacheKey(strings.TrimPrefix(strings.TrimSpace(downstreamAuthorizationHeader(c.Request)), "Bearer "), nil)
	parts := []string{nativeWSCacheOwner(owner, scope.identity), responsesWSSessionPreemptScopeHash(c, scope.identity),
		responsesWSTransportLane(c, scope.body, scope.identity), gjson.GetBytes(scope.body, "stream_id").String(), keyIdentity, scope.databaseScope}
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return nativeWSCachePrefix + hex.EncodeToString(hash[:])
}

func nativeWSLocalLookup(owner, responseID string) responseCacheLookupResult {
	key := responseCacheStoreKey(owner, responseID)
	respCache.mu.Lock()
	defer respCache.mu.Unlock()
	if entry := respCache.store[key]; entry != nil && time.Now().Before(entry.expiresAt) {
		respCache.lru.MoveToFront(entry.element)
		return responseCacheLookupResult{Kind: responseCacheLookupHit, Source: responseCacheSourceLocal,
			Items: append([]json.RawMessage(nil), entry.items...), nativeProvenance: entry.nativeProvenance}
	}
	if marker := respCache.markers[key]; marker != nil && time.Now().Before(marker.expiresAt) {
		return responseCacheLookupResult{Kind: marker.kind}
	}
	return responseCacheLookupResult{Kind: responseCacheLookupMiss}
}

// 终态暴露前建立屏障，跨下游重连须等提交完成再读取快照。
func beginNativeWSCommit(owner, responseID string) func() {
	if !strings.HasPrefix(owner, nativeWSCachePrefix) || responseID == "" {
		return func() {}
	}
	key := responseCacheStoreKey(owner, responseID)
	nativeWSCommits.Lock()
	if nativeWSCommits.pending[key] != nil || len(nativeWSCommits.pending) >= nativeWSCommitMaxEntries {
		nativeWSCommits.Unlock()
		return func() {}
	}
	done := make(chan struct{})
	nativeWSCommits.pending[key] = done
	nativeWSCommits.Unlock()
	finishShared := beginNativeWSSharedCommit(key)
	var once sync.Once
	return func() {
		once.Do(func() {
			finishShared()
			nativeWSCommits.Lock()
			delete(nativeWSCommits.pending, key)
			close(done)
			nativeWSCommits.Unlock()
		})
	}
}

func waitNativeWSCommit(ctx context.Context, owner, responseID string) error {
	nativeWSCommits.Lock()
	done := nativeWSCommits.pending[responseCacheStoreKey(owner, responseID)]
	nativeWSCommits.Unlock()
	if done == nil {
		return waitNativeWSSharedCommit(ctx, owner, responseID)
	}
	timer := time.NewTimer(nativeWSCommitWait)
	defer timer.Stop()
	select {
	case <-done:
		return waitNativeWSSharedCommit(ctx, owner, responseID)
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return &ResponsesContinuationLostError{Reason: "context_commit_pending"}
	}
}

func nativeWSReplayItem(item gjson.Result) (json.RawMessage, *api.APIError) {
	return nativeWSReplayItemChecked(item, false)
}

func nativeWSReplayItemChecked(item gjson.Result, allowOpaque bool) (json.RawMessage, *api.APIError) {
	if (item.Get("encrypted_content").String() != "" && !allowOpaque) || item.Get("type").String() == "item_reference" {
		return nil, nativeResponsesWSContextError(responsesWSContextUnavailable(http.StatusConflict, "nonportable_context"))
	}
	raw, ok := normalizeReplayableCachedFunctionCall(json.RawMessage(item.Raw))
	if !ok {
		return nil, nativeResponsesWSContextError(responsesWSContextUnavailable(http.StatusConflict, "invalid_context_item"))
	}
	raw, ok = stripResponseItemID(raw)
	if !ok {
		return nil, nativeResponsesWSContextError(responsesWSContextUnavailable(http.StatusConflict, "invalid_context_item"))
	}
	return raw, nil
}

type nativeWSCompletedContext struct {
	owner      string
	input      string
	completed  []byte
	outputs    []json.RawMessage
	provenance *nativeWSReplayProvenance
}

type nativeWSReplayProvenance struct {
	accountID  int64
	generation int64
	domain     string
}

func nativeWSAccountProvenance(account *auth.Account) *nativeWSReplayProvenance {
	if account == nil {
		return nil
	}
	return &nativeWSReplayProvenance{accountID: account.ID(), generation: account.GetCredentialGeneration(), domain: accountCompactionDomain(account)}
}

func (provenance *nativeWSReplayProvenance) matches(current *nativeWSReplayProvenance) bool {
	return provenance != nil && current != nil && *provenance == *current
}

func cacheNativeWSContext(snapshot nativeWSCompletedContext) {
	owner, input, completed, outputs := snapshot.owner, snapshot.input, snapshot.completed, snapshot.outputs
	responseID := gjson.GetBytes(completed, "response.id").String()
	if input == "" || responseID == "" {
		return
	}
	var items []json.RawMessage
	if json.Unmarshal([]byte(input), &items) != nil {
		return
	}
	terminal := gjson.GetBytes(completed, "response.output").Array()
	if len(terminal) > len(outputs) {
		outputs = nil
		for _, item := range terminal {
			outputs = append(outputs, json.RawMessage(item.Raw))
		}
	}
	// 保留不透明输出，恢复时核对账号兼容性；不能丢掉必要推理后声称上下文完整。
	items = mergeResponsesWSContext(items, outputs)
	key := responseCacheStoreKey(owner, responseID)
	items, admitted, _, serial := admitResponseCacheWithTicket(key, items)
	respCache.mu.RLock()
	expiresAt := time.Now().Add(respCache.config.ttl)
	respCache.mu.RUnlock()
	setNativeWSLocalMetadata(key, nativeWSLocalMetadata{serial: serial, provenance: snapshot.provenance, expiresAt: expiresAt})
	record := nativeWSSharedContext{Version: nativeWSContextVersion, Items: items, ExpiresAt: expiresAt}
	if snapshot.provenance != nil {
		record.AccountID, record.Generation, record.Domain = snapshot.provenance.accountID, snapshot.provenance.generation, snapshot.provenance.domain
	}
	if !admitted && items == nil || len(record.Domain) > nativeWSDomainMaxBytes {
		record.Items, record.Domain, record.Unavailable = nil, "", "oversize"
	}
	persistNativeWSContext(nativeWSSharedWrite{key: key, response: responseID, serial: serial, record: record})
}

func cacheCommittedResponsesWSContext(snapshot nativeWSCompletedContext) {
	if strings.HasPrefix(snapshot.owner, nativeWSCachePrefix) {
		cacheNativeWSContext(snapshot)
		return
	}
	cacheResponsesWSCompletedResponse(snapshot.owner, snapshot.input, snapshot.completed, snapshot.outputs)
}

func (s *responsesWSReplaySource) setAccount(account *auth.Account) {
	if s != nil {
		s.attemptProvenance = nativeWSAccountProvenance(account)
	}
}

func (s *responsesWSReplaySource) nativeReplayProvenance() *nativeWSReplayProvenance {
	if s.previous != nil {
		return s.previous.nativeProvenance
	}
	if s.recoveryProvenance != nil {
		return s.recoveryProvenance
	}
	return s.attemptProvenance
}

func (s *responsesWSReplaySource) nativeValidatedInput() string {
	if strings.Contains(s.input, `"encrypted_content"`) && !s.nativeReplayProvenance().matches(s.attemptProvenance) {
		s.err = nativeResponsesWSContextError(responsesWSContextUnavailable(http.StatusConflict, "incompatible_encrypted_context"))
		return ""
	}
	return s.input
}

func (s *responsesWSReplaySource) committedProvenance() *nativeWSReplayProvenance {
	if s == nil {
		return nil
	}
	return s.attemptProvenance
}

func newResponsesWSReplaySourceFromRecovery(input string, source *responsesWSReplaySource) *responsesWSReplaySource {
	replay := newResponsesWSReplaySourceFromInput(input)
	if source != nil && strings.Contains(input, `"encrypted_content"`) {
		replay.recoveryProvenance = source.nativeReplayProvenance()
	}
	return replay
}
