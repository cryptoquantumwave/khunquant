package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// GET /api/sandbox/timeline is the launcher's read-only view of a locked instance's lab
// journal. It exists only in a locked process and is guarded exactly as the dashboard
// reads are (loopback, bearer, no browser headers, a configured token), but with the
// instance's VIEW token, never the lab instance token: under the lock the management
// token of the running process is the instance token (enforceLockedConfig), so the
// launcher, which reads management_token from <home>/config.json, holds a token that
// reads this one route and nothing else. See captureLabViewToken.

const (
	timelinePayloadMax = 4 << 10 // bytes of one entry's payload
	timelineDefault    = 200
	timelineMaxLimit   = 500
)

var (
	timelineKindsRe    = regexp.MustCompile(`^[a-z_.*]{1,32}(,[a-z_.*]{1,32}){0,15}$`)
	timelineAfterSeqRe = regexp.MustCompile(`^[0-9]{1,18}$`)
)

var (
	viewTokenMu sync.RWMutex
	viewToken   string
)

// captureLabViewToken keeps the management token the instance's config file holds, before
// enforceLockedConfig replaces it with the instance token, as the one token the timeline
// accepts. It never keeps the instance token or an empty one. It is called on every load
// and reload, so a reloaded file re-arms it (or disarms it when the token is gone).
func captureLabViewToken(fileToken, instanceToken string) {
	if fileToken == instanceToken {
		fileToken = ""
	}
	viewTokenMu.Lock()
	viewToken = fileToken
	viewTokenMu.Unlock()
}

func labViewToken() string {
	viewTokenMu.RLock()
	defer viewTokenMu.RUnlock()
	return viewToken
}

type timelineEntry struct {
	Seq       int64           `json:"seq"`
	SimTime   time.Time       `json:"sim_time"`
	WallTime  time.Time       `json:"wall_time"`
	Kind      string          `json:"kind"`
	Source    string          `json:"source"`
	Subject   string          `json:"subject"`
	Summary   string          `json:"summary"`
	Payload   json.RawMessage `json:"payload"` // at most timelinePayloadMax bytes
	Truncated bool            `json:"truncated,omitempty"`
}

type timelineResponse struct {
	Locked   bool            `json:"locked"` // always true: the route exists only when locked
	Instance string          `json:"instance"`
	Entries  []timelineEntry `json:"entries"`
	NextSeq  int64           `json:"next_seq"`
	Drops    uint64          `json:"drops"`
}

// registerSandboxTimeline mounts the read route. Only GET is registered.
func registerSandboxTimeline(mux handleMux, j *labjournal.Journal, token string) {
	h := requireManagementToken(token, func(w http.ResponseWriter, r *http.Request) { serveTimeline(w, r, j, token) })
	mux.Handle("GET /api/sandbox/timeline", loopbackOnly(bearerTokenMiddleware(token, noBrowser(h))))
}

func serveTimeline(w http.ResponseWriter, r *http.Request, j *labjournal.Journal, viewTok string) {
	q := r.URL.Query()
	after, limit := int64(0), timelineDefault
	if v := q.Get("after_seq"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || !timelineAfterSeqRe.MatchString(v) {
			manageError(w, http.StatusBadRequest, "after_seq is not valid")
			return
		}
		after = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > timelineMaxLimit {
			manageError(w, http.StatusBadRequest, "limit is not valid")
			return
		}
		limit = n
	}
	var kinds []string
	if v := q.Get("kinds"); v != "" {
		if !timelineKindsRe.MatchString(v) {
			manageError(w, http.StatusBadRequest, "kinds is not valid")
			return
		}
		kinds = strings.Split(v, ",")
	}
	es, err := j.Query(r.Context(), labjournal.Query{Kinds: kinds, AfterSeq: after, Limit: limit})
	if err != nil {
		manageError(w, http.StatusInternalServerError, "the journal could not be read")
		return
	}
	lk, _ := sandbox.LockInfo()
	resp := timelineResponse{Locked: true, Instance: lk.InstanceName, Entries: make([]timelineEntry, 0, len(es)),
		NextSeq: after, Drops: j.Drops()}
	mask := func(b []byte) []byte {
		for _, tok := range []string{lk.Token, viewTok} {
			if tok != "" {
				b = bytes.ReplaceAll(b, []byte(tok), []byte("***"))
			}
		}
		return b
	}
	for _, e := range es {
		// Everything the summary and the payload are cut from is masked before the cut, so
		// a token across a cut is not left as a prefix; the encoded body is masked again below.
		e.Payload = mask(e.Payload)
		e.Kind, e.Subject = string(mask([]byte(e.Kind))), string(mask([]byte(e.Subject)))
		p, cut := capPayload(e.Payload)
		resp.Entries = append(resp.Entries, timelineEntry{Seq: e.Seq, SimTime: e.SimTime.UTC(), WallTime: e.WallTime.UTC(),
			Kind: e.Kind, Source: e.Source, Subject: e.Subject, Summary: labjournal.Summary(e), Payload: p, Truncated: cut})
		resp.NextSeq = max(resp.NextSeq, e.Seq)
	}
	// Every other journal read masks the instance token (the supervisor does, on the
	// encoded answer); this route is read straight from the gateway, so it masks here: both
	// the instance token and the view token, wherever free text put them (218-3).
	body, err := json.Marshal(resp)
	if err != nil {
		manageError(w, http.StatusInternalServerError, "the journal could not be encoded")
		return
	}
	body = mask(body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}

// capPayload returns p when it fits, else the start of its text as one JSON string (a
// cut document is not valid JSON), cut at a character boundary, whose encoded size is at
// most timelinePayloadMax.
func capPayload(p json.RawMessage) (json.RawMessage, bool) {
	if len(p) <= timelinePayloadMax {
		return p, false
	}
	for n := timelinePayloadMax - 2; n > 0; n = n * 3 / 4 { // escaping can grow the cut text
		if b, err := json.Marshal(strings.ToValidUTF8(string(p[:n]), "")); err == nil && len(b) <= timelinePayloadMax {
			return b, true
		}
	}
	return json.RawMessage(`""`), true
}
