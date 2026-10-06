package provider

// ZCode's marketing-plan claim (the daily / one-off trust-build / gift
// plans ZCode lists on each sign-in, #1038 / Discord): the preview is
// read, every plan it lists that the balance doesn't already hold is
// claimed through the plugin's fetch. The plugin signs the request on
// the account's JWT, with the captcha header ZCode's own dialog sends
// (the silent captcha attestation, an empty X-Aliyun-Captcha-Verify-Param;
// ZCode's own claimManualPlan log pair goes through without a human
// slider). A refused sign-in is an answer for the day; asked again the
// next day, the same day nothing is asked.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeZCodeClaim struct {
	mu         sync.Mutex
	expired    bool
	code       int         // shared code for preview + claim
	previewOK  bool        // the preview lists a claimable plan
	planID     string      // the plan the preview returns
	granted    float64     // the credit the claim returns
	claimCode  int         // overrides code for the claim endpoint
	asked      []string    // GET/POST path asked
	bodyErrs   []string    // body shapes the test rejects
}

func (f *fakeZCodeClaim) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var b []byte
		if r.Body != nil {
			b, _ = io.ReadAll(r.Body)
		}
		f.asked = append(f.asked, r.Method+" "+r.URL.Path)
		if f.expired {
			w.Header().Set("X-Magpie-Sign-In", "expired")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET " + zcodeClaimPreview:
			if len(b) != 0 {
				f.bodyErrs = append(f.bodyErrs, "a GET with a body")
			}
			plans := []map[string]any{}
			if f.previewOK {
				plans = append(plans, map[string]any{
					"plan_id": f.planID,
					"name":    "ZCode Trust Build",
					"status":  "active",
				})
			}
			env := map[string]any{"code": f.code, "msg": "", "data": map[string]any{"plans": plans}}
			_ = json.NewEncoder(w).Encode(env)
		case "POST " + zcodeClaimClaim:
			var req zcodeClaimRequest
			if err := json.Unmarshal(b, &req); err != nil {
				f.bodyErrs = append(f.bodyErrs, "claim not JSON: "+string(b))
			}
			if req.PlanID == "" {
				f.bodyErrs = append(f.bodyErrs, "claim without plan_id")
			}
			cc := f.code
			if f.claimCode != 0 {
				cc = f.claimCode
			}
			env := map[string]any{
				"code": cc, "msg": "",
				"data": map[string]any{"grant_units": f.granted, "total_units": f.granted},
			}
			_ = json.NewEncoder(w).Encode(env)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	old := zcodeClaimBaseURL
	zcodeClaimBaseURL = srv.URL
	t.Cleanup(func() { zcodeClaimBaseURL = old })
	return srv
}

func (f *fakeZCodeClaim) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.asked
	f.asked = nil
	return out
}

// ZCode's daily claim: the preview first, every listed plan's claim
// while there is one, the next day finds nothing to do (the same day
// nothing is asked; an expired sign-in is a failure with its reason).
func TestZCodeClaimOncePerDay(t *testing.T) {
	f := &fakeZCodeClaim{previewOK: true, planID: "zcode-v3-start-plan-trust-1006", granted: 100_000_000}
	f.serve(t)
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	c := newZCodeCheckiner(func() []zcodeClaimAcct {
		return []zcodeClaimAcct{{User: "197", On: true, uid: "uid-1", via: http.DefaultClient.Do}}
	})
	c.path = filepath.Join(t.TempDir(), "zcode-checkin.json")
	c.now = func() time.Time { return now }

	rs := c.checkinNow(t.Context(), false)
	if len(rs) != 1 || rs[0].Outcome != CheckinClaimed || rs[0].Credit != 100_000_000 || rs[0].User != "197" || rs[0].By != "zcode" || !rs[0].Asked {
		t.Fatalf("claimable: %+v", rs)
	}
	if got := strings.Join(f.take(), ", "); got != "GET /api/v1/zcode-plan/billing/preview, POST /api/v1/zcode-plan/billing/claim" {
		t.Fatalf("asked %s", got)
	}

	// the day's answer holds: nothing asked again
	if rs := c.checkinNow(t.Context(), false); rs[0].Outcome != CheckinClaimed || rs[0].Asked || len(f.take()) != 0 {
		t.Fatalf("asked again the same day: %+v", rs)
	}

	// the next day, preview returns nothing (already held)
	f.previewOK = false
	now = now.Add(24 * time.Hour)
	if rs := c.checkinNow(t.Context(), false); rs[0].Outcome != CheckinInactive {
		t.Fatalf("in already: %+v", rs)
	}
	if got := strings.Join(f.take(), ", "); got != "GET /api/v1/zcode-plan/billing/preview" {
		t.Fatalf("in already, asked %s", got)
	}
}

// An empty preview (no plan to claim today) is the inactive answer, not
// a failure.
func TestZCodeClaimNoPlanToday(t *testing.T) {
	f := &fakeZCodeClaim{previewOK: false}
	f.serve(t)
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	c := newZCodeCheckiner(func() []zcodeClaimAcct {
		return []zcodeClaimAcct{{User: "133", On: true, uid: "uid-2", via: http.DefaultClient.Do}}
	})
	c.path = filepath.Join(t.TempDir(), "zcode-checkin.json")
	c.now = func() time.Time { return now }
	if rs := c.checkinNow(t.Context(), false); rs[0].Outcome != CheckinInactive {
		t.Fatalf("no plan: %+v", rs)
	}
	if got := f.take(); len(got) != 1 || got[0] != "GET /api/v1/zcode-plan/billing/preview" {
		t.Fatalf("asked %v", got)
	}
}

// An expired sign-in is a failure with its reason, tried again.
func TestZCodeClaimExpiredSignIn(t *testing.T) {
	f := &fakeZCodeClaim{expired: true}
	f.serve(t)
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	c := newZCodeCheckiner(func() []zcodeClaimAcct {
		return []zcodeClaimAcct{{User: "133", On: true, uid: "uid-3", via: http.DefaultClient.Do}}
	})
	c.path = filepath.Join(t.TempDir(), "zcode-checkin.json")
	c.now = func() time.Time { return now }

	rs := c.checkinNow(t.Context(), false)
	if rs[0].Outcome != CheckinFailed || !strings.Contains(rs[0].Msg, "sign-in has expired") {
		t.Fatalf("expired: %+v", rs)
	}
	f.expired = false
	f.previewOK = true
	f.planID = "zcode-v3-start-plan-trust-1006"
	f.granted = 100_000_000
	// a refused token, the second ask gets through (a magpie retry)
	if rs := c.checkinNow(t.Context(), true); rs[0].Outcome != CheckinClaimed {
		t.Fatalf("expired, retry: %+v", rs)
	}
}

// A claim refusal on one plan short-circuits the rest, the day's
// answer is the failure (magpie retries the whole account tomorrow).
func TestZCodeClaimRefused(t *testing.T) {
	f := &fakeZCodeClaim{previewOK: true, planID: "zcode-v3-start-plan-trust-1006"}
	f.serve(t)
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	c := newZCodeCheckiner(func() []zcodeClaimAcct {
		return []zcodeClaimAcct{{User: "197", On: true, uid: "uid-4", via: http.DefaultClient.Do}}
	})
	c.path = filepath.Join(t.TempDir(), "zcode-checkin.json")
	c.now = func() time.Time { return now }
	// preview returns 200 with the plan; claim returns 9002 (refused)
	// we swap the handler by re-serving with claimCode set; simpler: have
	// the fake set code per-call. f.code is shared across both endpoints,
	// so we route by path instead.
	f.code = 0
	// override the claim answer: return 9002 always by hitting a second
	// handler mounted alongside. we use code claim-side via a hook: ask
	// twice — preview returns the plan, claim returns the refusal.
	// Since fake.code applies to both, we set up a per-request trick:
	// set code to the refusal; preview path reads it but returns the
	// envelope; the test asserts the day's outcome is ineligibled.
	// Easier: claim path uses claimCode too, see below.
	f.code = 0
	f.claimCode = 9002
	f.granted = 0
	if rs := c.checkinNow(t.Context(), false); rs[0].Outcome != CheckinIneligible {
		t.Fatalf("refused: %+v", rs)
	}
}

// no ZCode account is signed in: nothing comes back, no fetch is asked.
func TestZCodeClaimNoAccount(t *testing.T) {
	f := &fakeZCodeClaim{}
	f.serve(t)
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	c := newZCodeCheckiner(func() []zcodeClaimAcct { return nil })
	c.path = filepath.Join(t.TempDir(), "zcode-checkin.json")
	c.now = func() time.Time { return now }
	if rs := c.checkinNow(t.Context(), false); len(rs) != 0 {
		t.Fatalf("no account: %+v", rs)
	}
	if got := f.take(); len(got) != 0 {
		t.Fatalf("asked %v", got)
	}
}