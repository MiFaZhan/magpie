package provider

// ZCode's marketing-plan claim (the daily / one-off trust-build / gift
// plans ZCode lists on each sign-in, #1038 / Discord): ZCode is served
// only by its community plugin, @magpie-community/opencode-zcode-auth
// (provider "zcode"; no built-in, no mover). While the setting is on,
// whichever magpie runs the gateway asks the plugin's fetch for the
// claim preview and then for each unclaimed plan's claim, as ZCode's
// settings dialog does: it asks GET /api/v1/zcode-plan/billing/preview on
// zcode.z.ai and POSTs /api/v1/zcode-plan/billing/claim with the plan id
// for each plan the preview lists that the account's balance doesn't
// already hold. Both go through the plugin's fetch (0.1.11 and later),
// which sends them as the account on the JWT, with the captcha header
// ZCode's own dialog sends (the silent captcha attestation, an empty
// X-Aliyun-Captcha-Verify-Param: ZCode's own claimManualPlan log pair
// goes through without a human slider).
//
// What came of it is kept in zcode-checkin.json by account id, as the
// other check-ins are (checkin.go): an answer holds for the day, a
// request that got none is tried again later.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
)

// ZCodeID is the ZCode plugin's provider.
const ZCodeID = "zcode"

// zcodeClaimBaseURL is ZCode's claim API on zcode.z.ai.
var zcodeClaimBaseURL = "https://zcode.z.ai"

// zcodeClaimPreview is the GET path that lists the account's manual-claim
// plans; zcodeClaimClaim is the POST path that takes one.
const (
	zcodeClaimPreview = "/api/v1/zcode-plan/billing/preview"
	zcodeClaimClaim   = "/api/v1/zcode-plan/billing/claim"
)

// zcodeClaimAcct is a ZCode plugin account checked in: via sends a
// request as the account through the plugin's fetch (which signs it on
// the JWT). It is set by the production path to a plugin.Fetch-backed
// function; tests pass http.DefaultClient.Do against a local httptest
// server, as the other check-ins do.
type zcodeClaimAcct struct {
	User string
	On   bool
	acct plugin.Account
	uid  string
	via  func(*http.Request) (*http.Response, error)
}

func (a zcodeClaimAcct) key() string {
	if a.uid == "" {
		return ""
	}
	return ZCodeID + "|" + a.uid
}

// zcodeCheckinPath is the file the day's outcome is kept in, by account id.
func zcodeCheckinPath() string {
	return filepath.Join(filepath.Dir(Path()), "zcode-checkin.json")
}

var zcodeCheckinMu sync.Mutex

func newZCodeCheckiner(accounts func() []zcodeClaimAcct) checkiner {
	return checkiner{path: zcodeCheckinPath(), now: time.Now, mu: &zcodeCheckinMu, by: "zcode", label: "zcode", accounts: func() []checkinAcct {
		var out []checkinAcct
		for _, a := range accounts() {
			out = append(out, checkinAcct{User: a.User, On: a.On, key: a.key(), do: func(ctx context.Context) WorkBuddyCheckin { return zcodeCheckin(ctx, a) }})
		}
		return out
	}}
}

// zcodeCheckin asks ZCode's marketing-plan preview for the account, and
// claims every plan the preview lists that the account's balance doesn't
// already hold (a claim is a one-shot entitlement, the same plan_id
// claimed twice is a server-side no-op). The preview's plan_id is also
// what ZCode's own marketing-claim dialog sends.
func zcodeCheckin(ctx context.Context, a zcodeClaimAcct) WorkBuddyCheckin {
	plans, err := zcodeClaimPreviewCall(ctx, a)
	if err != nil {
		return wbCheckinFailed(err)
	}
	if len(plans) == 0 {
		return WorkBuddyCheckin{Outcome: CheckinInactive}
	}
	out := WorkBuddyCheckin{Outcome: CheckinDone}
	for _, p := range plans {
		r := zcodeClaimCall(ctx, a, p.PlanID)
		if r.Outcome == CheckinFailed || r.Outcome == CheckinIneligible {
			return r
		}
		if r.Outcome == CheckinClaimed {
			out.Outcome = CheckinClaimed
			out.Credit += r.Credit
		}
	}
	return out
}

// zcodeClaimPreviewCall asks ZCode's preview for the account's manual-
// claim plans.
func zcodeClaimPreviewCall(ctx context.Context, a zcodeClaimAcct) ([]zcodePreviewPlan, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, zcodeClaimBaseURL+zcodeClaimPreview, nil)
	if err != nil {
		return nil, err
	}
	res, err := a.via(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode == http.StatusUnauthorized || strings.EqualFold(res.Header.Get("X-Magpie-Sign-In"), "expired") {
		return nil, errors.New("ZCode's sign-in has expired — sign in again")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, &accountStatusError{status: res.StatusCode}
	}
	var env zcodePreviewEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("zcode preview code %d: %s", env.Code, env.Msg)
	}
	return env.Data.Plans, nil
}

// zcodeClaimCall asks ZCode's claim for one plan id.
func zcodeClaimCall(ctx context.Context, a zcodeClaimAcct, planID string) WorkBuddyCheckin {
	body, _ := json.Marshal(zcodeClaimRequest{PlanID: planID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, zcodeClaimBaseURL+zcodeClaimClaim, strings.NewReader(string(body)))
	if err != nil {
		return wbCheckinFailed(err)
	}
	res, err := a.via(req)
	if err != nil {
		return wbCheckinFailed(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode == http.StatusUnauthorized || strings.EqualFold(res.Header.Get("X-Magpie-Sign-In"), "expired") {
		return WorkBuddyCheckin{Outcome: CheckinFailed, Msg: "ZCode's sign-in has expired — sign in again"}
	}
	if res.StatusCode == http.StatusTooManyRequests {
		return WorkBuddyCheckin{Outcome: CheckinFailed, Msg: "asked too often; try again later"}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return WorkBuddyCheckin{Outcome: CheckinFailed, Msg: fmt.Sprintf("HTTP %d", res.StatusCode)}
	}
	var env zcodeClaimEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return wbCheckinFailed(err)
	}
	switch env.Code {
	case 0:
		// the trust-build plan gives 100,000,000 GLM-5.3-Flash tokens
		// (zcode-v3-start-plan-trust-1006), so the credit is the grant_units
		// the entitlement says it has, when the server tells it. Otherwise
		// the server returns a refilled balance with a per-plan entitlement;
		// the grant_units is the count the claim added.
		credit := env.Data.GrantUnits
		if credit == 0 {
			credit = env.Data.TotalUnits
		}
		if env.Data.AlreadyClaimed {
			return WorkBuddyCheckin{Outcome: CheckinDone, Credit: credit}
		}
		return WorkBuddyCheckin{Outcome: CheckinClaimed, Credit: credit}
	case zcodeClaimAlreadyHeld:
		return WorkBuddyCheckin{Outcome: CheckinDone}
	case zcodeClaimNotEligible:
		return WorkBuddyCheckin{Outcome: CheckinIneligible}
	default:
		return WorkBuddyCheckin{Outcome: CheckinFailed, Msg: fmt.Sprintf("code %d: %s", env.Code, env.Msg)}
	}
}

// zcodePreviewEnvelope is the GET /billing/preview's body (the JSON the
// ZCode client reads).
type zcodePreviewEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Plans []zcodePreviewPlan `json:"plans"`
	} `json:"data"`
}

// zcodePreviewPlan is one entry the preview lists; the plugin's own
// routing treats plan_id as the key to claim.
type zcodePreviewPlan struct {
	PlanID string `json:"plan_id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// zcodeClaimRequest is the body ZCode sends on POST /billing/claim. The
// plugin's request shape mirrors ZCode's own (see packages/zcode/index.mjs
// claimManualPlan): { plan_id }.
type zcodeClaimRequest struct {
	PlanID string `json:"plan_id"`
}

// zcodeClaimEnvelope is the POST /billing/claim's body. The grant_units
// the claim added (alreadyClaimed = true when the server says nothing
// changed; the plugin's preview then knows to skip next time).
type zcodeClaimEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		GrantUnits     float64 `json:"grant_units"`
		TotalUnits     float64 `json:"total_units"`
		AlreadyClaimed bool    `json:"already_claimed"`
	} `json:"data"`
}

// zcodeClaim's codes the answer may carry.
const (
	zcodeClaimAlreadyHeld = 9001 // server: this account already has this plan
	zcodeClaimNotEligible = 9002 // server: this account can't claim this plan
)

// zcodeVia is the production via: it sends the request to the ZCode
// plugin's fetch as the account, with the plugin adding the JWT and the
// captcha header. Set on each account by zcodeClaimAccounts (0.1.11 and
// later serves the preview / claim paths).
func zcodeVia(req *http.Request) (*http.Response, error) {
	pp, ok := PluginOf(ZCodeID)
	if !ok {
		return nil, errors.New("the ZCode plugin is not installed")
	}
	h := map[string]string{}
	for k, vs := range req.Header {
		if len(vs) > 0 {
			h[strings.ToLower(k)] = vs[0]
		}
	}
	return plugin.Fetch(req.Context(), plugin.FetchRequest{
		Provider: pp.ID,
		URL:     req.URL.String(),
		Method:  req.Method,
		Headers: h,
		Body:    bodyOf(req),
	})
}

// bodyOf is req's body as bytes, nil when it has none.
func bodyOf(req *http.Request) []byte {
	if req.Body == nil {
		return nil
	}
	b, _ := io.ReadAll(req.Body)
	return b
}

// zcodeClaimAccounts are the ZCode plugin's accounts, each sent through
// the plugin's fetch, which signs it on the JWT.
func zcodeClaimAccounts() []zcodeClaimAcct {
	pp, ok := PluginOf(ZCodeID)
	if !ok {
		return nil
	}
	var out []zcodeClaimAcct
	for _, l := range pluginLogins(pp) {
		uid := l.acct.Key
		out = append(out, zcodeClaimAcct{User: l.User, On: l.On, uid: uid, acct: l.acct, via: zcodeVia})
	}
	return out
}

// CheckInZcode checks each ZCode plugin account in use in for today now,
// those not in yet, and says how each stands.
func CheckInZcode(ctx context.Context) []WorkBuddyCheckin {
	return newZCodeCheckiner(zcodeClaimAccounts).checkinNow(ctx, true)
}

// ZCodeCheckins is each ZCode account's last check-in, as kept, by its
// name now; asks nothing.
func ZCodeCheckins() []WorkBuddyCheckin {
	st := readCheckins(zcodeCheckinPath())
	var out []WorkBuddyCheckin
	listed := map[string]bool{}
	for _, a := range zcodeClaimAccounts() {
		key := a.key()
		if r, ok := st[key]; ok && a.On && key != "" && !listed[key] {
			listed[key] = true
			r.User, r.By = a.User, "zcode"
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out
}

// HasZCode says whether a ZCode account is signed in.
func HasZCode() bool { return len(zcodeClaimAccounts()) > 0 }

// zcodeCards are the usage cards of accts, for WithCheckins.
func zcodeCards(accts []zcodeClaimAcct) []checkinCard {
	var out []checkinCard
	for _, a := range accts {
		if key := a.key(); a.On && key != "" {
			out = append(out, checkinCard{User: a.User, card: ZCodeID, key: key})
		}
	}
	return out
}

// KeepZCodeClaimedIn checks the ZCode accounts in each day while
// settings say to, as KeepWorkBuddyCheckedIn does WorkBuddy's.
func KeepZCodeClaimedIn(ctx context.Context) {
	keepCheckedIn(ctx, "zcode", newZCodeCheckiner(zcodeClaimAccounts), func() bool { return settings.Load().WorkBuddyCheckin })
}