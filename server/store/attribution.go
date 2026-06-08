package store

import (
	"context"
	"sort"
	"time"
)

// DeviceShare is one device's attributed contribution to an account's usage,
// expressed in utilization points (0–100, same unit as the account's usage
// bars) for the CURRENT window of each rate-limit window. Across all devices
// plus AccountAttribution.Unattributed, the per-window points sum to roughly
// the account's current utilization in that window — "roughly" because
// consumption that happened before the first observed snapshot (e.g. right
// after a daemon restart) can't be attributed and is silently absent.
type DeviceShare struct {
	DeviceID       string
	DeviceName     string
	FiveHour       float64
	SevenDay       float64
	SevenDaySonnet float64
	// LastUsedAt is the approximate last moment this device actually drove
	// usage on the account: the end of the most recent interval (across any
	// window) where utilization rose AND this device held the lease. Unlike
	// accounts.last_used_at (a credential-switch timestamp) or a lease's
	// acquired_at (lease, not consumption), this is grounded in real
	// utilization deltas — a device that leased but never sent a request never
	// gets a LastUsedAt. Interval-granular by construction (bounded by the
	// usage poll cadence and UsageHistoryRetention). 0 means never observed.
	LastUsedAt int64
}

// AccountAttribution is the per-device breakdown for one account. Devices is
// sorted by total contributed points descending (the device that drove the
// most usage first). Unattributed carries the points that fell in an interval
// with no lease holder (DeviceID/DeviceName empty).
type AccountAttribution struct {
	AccountID    int64
	Devices      []DeviceShare
	Unattributed DeviceShare
	// SampleCount is how many usage snapshots backed the computation and
	// SampleStart the oldest one's ts — lets the UI flag thin data ("only
	// started observing 10 min ago, shares may be incomplete").
	SampleCount int
	SampleStart int64
}

// ComputeAttribution replays this account's usage_history deltas against its
// lease_events segments to estimate how much of each window's consumption each
// device drove. Reads up to UsageHistoryRetention of history; the per-window
// walk resets its accumulator whenever utilization drops (a window rollover),
// so the result reflects only the current window. Approximate by construction
// — see DeviceShare — but uses real held-time overlap, not a point-in-time guess.
func (s *Store) ComputeAttribution(ctx context.Context, accountID int64) (AccountAttribution, error) {
	now := time.Now().UnixMilli()
	since := time.Now().Add(-UsageHistoryRetention).UnixMilli()

	rows, err := s.UsageHistoryForAccountSince(ctx, accountID, since)
	if err != nil {
		return AccountAttribution{}, err
	}
	segs, err := s.LeaseEventsForAccountSince(ctx, accountID, since)
	if err != nil {
		return AccountAttribution{}, err
	}

	out := AccountAttribution{AccountID: accountID, SampleCount: len(rows)}
	if len(rows) > 0 {
		out.SampleStart = rows[0].Timestamp
	}
	if len(rows) < 2 {
		// Nothing to diff yet — no attributable deltas.
		return out, nil
	}

	// lastUsed accumulates across all three windows (and across window
	// resets): the latest interval-end at which each device drove real usage.
	lastUsed := map[string]int64{}
	fhAcc, fhUn := attributeWindow(rows, segs, now, func(r UsageHistoryRow) float64 { return r.FiveHourUtil }, lastUsed)
	sdAcc, sdUn := attributeWindow(rows, segs, now, func(r UsageHistoryRow) float64 { return r.SevenDayUtil }, lastUsed)
	ssAcc, ssUn := attributeWindow(rows, segs, now, func(r UsageHistoryRow) float64 { return r.SevenDaySonnetUtil }, lastUsed)

	names, err := s.deviceDisplayNames(ctx)
	if err != nil {
		return AccountAttribution{}, err
	}

	// Union of every device that contributed to any window.
	devIDs := map[string]struct{}{}
	for id := range fhAcc {
		devIDs[id] = struct{}{}
	}
	for id := range sdAcc {
		devIDs[id] = struct{}{}
	}
	for id := range ssAcc {
		devIDs[id] = struct{}{}
	}
	for id := range devIDs {
		out.Devices = append(out.Devices, DeviceShare{
			DeviceID:       id,
			DeviceName:     names[id],
			FiveHour:       fhAcc[id],
			SevenDay:       sdAcc[id],
			SevenDaySonnet: ssAcc[id],
			LastUsedAt:     lastUsed[id],
		})
	}
	sort.Slice(out.Devices, func(i, j int) bool {
		ti := out.Devices[i].FiveHour + out.Devices[i].SevenDay + out.Devices[i].SevenDaySonnet
		tj := out.Devices[j].FiveHour + out.Devices[j].SevenDay + out.Devices[j].SevenDaySonnet
		if ti != tj {
			return ti > tj
		}
		return out.Devices[i].DeviceID < out.Devices[j].DeviceID
	})
	out.Unattributed = DeviceShare{FiveHour: fhUn, SevenDay: sdUn, SevenDaySonnet: ssUn}
	return out, nil
}

// attributeWindow walks consecutive snapshots for one window. A non-negative
// delta is the consumption in that interval; a negative delta means the window
// reset between the two snapshots, so we discard the prior window's tally and
// treat the new (lower) level as fresh consumption in this interval. Each
// interval's consumption is split across devices by how long each held the
// lease during [prev.ts, cur.ts].
func attributeWindow(rows []UsageHistoryRow, segs []LeaseEvent, now int64, util func(UsageHistoryRow) float64, lastUsed map[string]int64) (map[string]float64, float64) {
	accum := map[string]float64{}
	unattr := 0.0
	for i := 1; i < len(rows); i++ {
		prev, cur := rows[i-1], rows[i]
		var contribution float64
		if d := util(cur) - util(prev); d < 0 {
			// Window rollover — start the current window fresh. lastUsed is
			// deliberately NOT reset: a rollover is an accounting boundary, not
			// evidence the device stopped consuming.
			accum = map[string]float64{}
			unattr = 0
			contribution = util(cur)
		} else {
			contribution = d
		}
		if contribution <= 0 {
			continue
		}
		distribute(contribution, prev.Timestamp, cur.Timestamp, segs, now, accum, &unattr, lastUsed)
	}
	return accum, unattr
}

// distribute splits `amount` utilization points across the devices that held
// the lease during [from, to], proportional to each device's overlap duration.
// When no segment overlaps the interval, the points are unattributable (a gap
// where some device used the account without an open lease event, or before
// observation began) and accrue to *unattr.
func distribute(amount float64, from, to int64, segs []LeaseEvent, now int64, accum map[string]float64, unattr *float64, lastUsed map[string]int64) {
	if to <= from {
		return
	}
	type overlap struct {
		dev   string
		dur   int64
		endTs int64 // end of this device's overlap with [from, to]
	}
	var ovs []overlap
	var total int64
	for _, sg := range segs {
		end := sg.EndedAt
		if end == 0 {
			end = now // still-open segment runs to the present
		}
		lo, hi := from, to
		if sg.StartedAt > lo {
			lo = sg.StartedAt
		}
		if end < hi {
			hi = end
		}
		if hi > lo {
			ovs = append(ovs, overlap{sg.DeviceID, hi - lo, hi})
			total += hi - lo
		}
	}
	if total == 0 {
		*unattr += amount
		return
	}
	for _, o := range ovs {
		accum[o.dev] += amount * float64(o.dur) / float64(total)
		// This device held the lease while consumption happened — record the
		// end of its overlap as its last real-usage time, keeping the latest.
		if o.endTs > lastUsed[o.dev] {
			lastUsed[o.dev] = o.endTs
		}
	}
}

// deviceDisplayNames maps device id → display name (name, falling back to
// hostname, matching the "in use by …" badge logic) so attribution rows show
// a human label instead of an opaque id.
func (s *Store) deviceDisplayNames(ctx context.Context) (map[string]string, error) {
	devs, err := s.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(devs))
	for _, d := range devs {
		name := d.Name
		if name == "" {
			name = d.Hostname
		}
		m[d.ID] = name
	}
	return m, nil
}
