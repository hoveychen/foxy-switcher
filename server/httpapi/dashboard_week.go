package httpapi

import (
	"sort"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// weekHours is both the history span and the forecast horizon of the 7-day
// chart. It matches store.UsageHistoryRetention, so the same-week forecast
// always has last week's samples to replay.
const weekHours = 7 * 24

// DashboardWeek is the 7d pool's history over the last 7 days plus a 7-day
// forecast, hourly, anchored at now. Pcts are weighted used / capacity * 100
// with the same rules as the 24h trend: Pro-equivalent weights, and an
// account at its 7d threshold counts as full.
type DashboardWeek struct {
	Capacity float64 `json:"capacity"` // Pro-equivalents, constant
	NowTS    int64   `json:"now_ts"`
	// Points are hourly: weekHours+1 history points ending at now (the last
	// one is now itself), then weekHours forecast points.
	Points []DashboardWeekPoint `json:"points"`
}

// DashboardWeekPoint is one hour of the 7-day chart. History points carry
// ActualPct; forecast points carry the two projections. The now point carries
// all three so the forecast lines start where history ends.
type DashboardWeekPoint struct {
	TS       int64 `json:"ts"`
	Forecast bool  `json:"forecast"`
	// ActualPct: the pool at this instant, each account at its latest sample.
	ActualPct float64 `json:"actual_pct"`
	// SameWeekPct: last week's pool consumption in the same hour replayed
	// onto today's accounts (lowest 7d utilization first, the selector's
	// order), with every account emptying at its own 7d reset.
	SameWeekPct float64 `json:"same_week_pct"`
	// IdlePct: no further consumption; only the resets release capacity.
	IdlePct float64 `json:"idle_pct"`
}

// sevenDayWeek builds the 7-day chart from the current accounts and the
// usage history since now-7d (ascending by ts). Only Claude accounts carry a
// 7d weight, so other providers drop out through planWeight.
func sevenDayWeek(accs []store.Account, rows []store.UsageHistoryRow, now time.Time) DashboardWeek {
	hourMs := int64(time.Hour / time.Millisecond)
	weekMs := int64(weekHours) * hourMs
	nowMs := now.UnixMilli()

	type acct struct {
		id       int64
		w, th    float64
		active   bool
		resetsAt int64 // 0 = no reset within the horizon
		level    float64
	}
	var pool []*acct
	byID := make(map[int64]*acct)
	var capacity float64
	for _, a := range accs {
		_, w7d := planWeight(a.Provider, a.RateLimitTier, a.SubscriptionType)
		if w7d == 0 {
			continue
		}
		th := a.SevenDayThreshold
		if th <= 0 {
			th = 100
		}
		ac := &acct{id: a.ID, w: w7d, th: th, active: a.Status == "active", level: a.SevenDayUtil}
		if t, err := time.Parse(time.RFC3339, a.SevenDayResetsAt); err == nil {
			if ms := t.UnixMilli(); ms > nowMs {
				// The following reset is R+7d, past the horizon.
				ac.resetsAt = ms
			} else {
				// Reset already passed but not yet re-polled: the window is empty.
				ac.level = 0
			}
		}
		pool = append(pool, ac)
		byID[a.ID] = ac
		capacity += w7d
	}

	pct := func(level func(*acct) float64) float64 {
		if capacity == 0 {
			return 0
		}
		var used float64
		for _, ac := range pool {
			u := level(ac)
			if u >= ac.th {
				u = 100
			}
			used += u / 100 * ac.w
		}
		return used / capacity * 100
	}

	// History: each account at its latest sample at or before the point,
	// seeded with its earliest in-window sample (today's value when it has
	// none). Along the way, record each sample's consumption in
	// Pro-equivalents; a drop is a reset, so the new value is all fresh use.
	latest := make(map[int64]float64, len(pool))
	for _, ac := range pool {
		latest[ac.id] = ac.level
	}
	seen := make(map[int64]bool)
	for _, r := range rows {
		if !seen[r.AccountID] {
			seen[r.AccountID] = true
			latest[r.AccountID] = r.SevenDayUtil
		}
	}
	type use struct {
		ts int64
		pe float64
	}
	var uses []use
	prev := make(map[int64]float64)
	hasPrev := make(map[int64]bool)
	for _, r := range rows {
		ac, ok := byID[r.AccountID]
		if !ok {
			continue
		}
		if hasPrev[r.AccountID] {
			d := r.SevenDayUtil - prev[r.AccountID]
			if d < 0 {
				d = r.SevenDayUtil
			}
			if d > 0 {
				uses = append(uses, use{r.Timestamp, d / 100 * ac.w})
			}
		}
		prev[r.AccountID], hasPrev[r.AccountID] = r.SevenDayUtil, true
	}

	week := DashboardWeek{Capacity: capacity, NowTS: nowMs}
	rowIdx := 0
	for i := weekHours; i >= 0; i-- {
		at := nowMs - int64(i)*hourMs
		for rowIdx < len(rows) && rows[rowIdx].Timestamp <= at {
			latest[rows[rowIdx].AccountID] = rows[rowIdx].SevenDayUtil
			rowIdx++
		}
		week.Points = append(week.Points, DashboardWeekPoint{
			TS:        at,
			ActualPct: pct(func(ac *acct) float64 { return latest[ac.id] }),
		})
	}
	nowPt := &week.Points[len(week.Points)-1]
	// The now point is the current account state (what the KPI card shows),
	// which also folds in a reset that passed since the last poll.
	nowPt.ActualPct = pct(func(ac *acct) float64 { return ac.level })
	nowPt.SameWeekPct, nowPt.IdlePct = nowPt.ActualPct, nowPt.ActualPct

	// Forecast, hour by hour: reset accounts whose reset fell in the hour,
	// then pour last week's consumption for the same hour into active
	// accounts below threshold, lowest first, each up to its threshold.
	// Consumption the pool can't absorb is dropped: it would be throttled.
	idle := make(map[int64]float64, len(pool))
	for _, ac := range pool {
		idle[ac.id] = ac.level
	}
	useIdx := 0
	for k := 1; k <= weekHours; k++ {
		at := nowMs + int64(k)*hourMs
		for _, ac := range pool {
			if ac.resetsAt != 0 && ac.resetsAt <= at {
				ac.level, idle[ac.id], ac.resetsAt = 0, 0, 0
			}
		}
		var pe float64
		for useIdx < len(uses) && uses[useIdx].ts <= at-weekMs {
			if uses[useIdx].ts > at-weekMs-hourMs {
				pe += uses[useIdx].pe
			}
			useIdx++
		}
		if pe > 0 {
			open := make([]*acct, 0, len(pool))
			for _, ac := range pool {
				if ac.active && ac.level < ac.th {
					open = append(open, ac)
				}
			}
			sort.Slice(open, func(i, j int) bool {
				if open[i].level != open[j].level {
					return open[i].level < open[j].level
				}
				return open[i].id < open[j].id
			})
			for _, ac := range open {
				if pe <= 0 {
					break
				}
				room := (ac.th - ac.level) / 100 * ac.w
				if pe >= room {
					// Land exactly on the threshold so the account reads full.
					ac.level = ac.th
					pe -= room
					continue
				}
				ac.level += pe / ac.w * 100
				pe = 0
			}
		}
		week.Points = append(week.Points, DashboardWeekPoint{
			TS:          at,
			Forecast:    true,
			SameWeekPct: pct(func(ac *acct) float64 { return ac.level }),
			IdlePct:     pct(func(ac *acct) float64 { return idle[ac.id] }),
		})
	}
	return week
}
