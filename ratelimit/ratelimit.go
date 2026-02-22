// Package ratelimit provides a simple window-based rate limiter.
package ratelimit

// Based on github.com/mjl-/mox/ratelimit.

import (
	"net"
	"sync"
	"time"
)

// Limiter is a simple rate limiter with one or more fixed windows, e.g. the
// last minute/hour/day/week, working on multiple classes/subnets of an IP.
type Limiter struct {
	sync.Mutex
	IPClasses [2][]int // For IPv4 and IPv6.
	WindowLimits []WindowLimit

	ipmasked     [][16]byte
}

// WindowLimit holds counters for one window, with limits for each IP class/subnet.
type WindowLimit struct {
	Window time.Duration
	Limits []int64 // Per class.

	time   uint32   // Time/Window.
	counts map[struct {
		Index    uint8
		IPMasked [16]byte
	}]int64
}

// Add attempts to consume "n" items from the rate limiter. If the total for this
// key and this interval would exceed limit, "n" is not counted and false is
// returned. If now represents a different time interval, all counts are reset.
func (l *Limiter) Add(ip net.IP, tm time.Time, n int64) bool {
	return l.checkAdd(true, ip, tm, n)
}

// CanAdd returns if n could be added to the limiter.
func (l *Limiter) CanAdd(ip net.IP, tm time.Time, n int64) bool {
	return l.checkAdd(false, ip, tm, n)
}

func (l *Limiter) ensureInit() {
	if l.ipmasked == nil {
		l.ipmasked = make([][16]byte, len(l.IPClasses[0]))
	}
}

func (l *Limiter) checkAdd(add bool, ip net.IP, tm time.Time, n int64) bool {
	l.Lock()
	defer l.Unlock()

	l.ensureInit()

	// First check.
	for i, pl := range l.WindowLimits {
		t := uint32(tm.UnixNano() / int64(pl.Window))

		if t > pl.time || pl.counts == nil {
			l.WindowLimits[i].time = t
			pl.counts = map[struct {
				Index    uint8
				IPMasked [16]byte
			}]int64{} // Used below.
			l.WindowLimits[i].counts = pl.counts
		}

		for j := range len(l.ipmasked) {
			if i == 0 {
				l.ipmasked[j] = l.maskIP(j, ip)
			}

			v := pl.counts[struct {
				Index    uint8
				IPMasked [16]byte
			}{uint8(j), l.ipmasked[j]}]
			if v+n > pl.Limits[j] {
				return false
			}
		}
	}
	if !add {
		return true
	}
	// Finally record.
	for _, pl := range l.WindowLimits {
		for j := range len(l.ipmasked) {
			pl.counts[struct {
				Index    uint8
				IPMasked [16]byte
			}{uint8(j), l.ipmasked[j]}] += n
		}
	}
	return true
}

// Reset sets the counter to 0 for key and ip, and subtracts from the ipmasked counts.
func (l *Limiter) Reset(ip net.IP, tm time.Time) {
	l.Lock()
	defer l.Unlock()

	l.ensureInit()

	// Prepare masked ip's.
	for i := range len(l.ipmasked) {
		l.ipmasked[i] = l.maskIP(i, ip)
	}

	for _, pl := range l.WindowLimits {
		t := uint32(tm.UnixNano() / int64(pl.Window))
		if t != pl.time || pl.counts == nil {
			continue
		}
		var n int64
		for j := range len(l.ipmasked) {
			k := struct {
				Index    uint8
				IPMasked [16]byte
			}{uint8(j), l.ipmasked[j]}
			if j == 0 {
				n = pl.counts[k]
			}
			if pl.counts != nil {
				pl.counts[k] -= n
			}
		}
	}
}

func (l *Limiter) maskIP(i int, ip net.IP) [16]byte {
	isv4 := ip.To4() != nil
	class := 0
	total := 32
	if !isv4 {
		class = 1
		total = 128
	}

	ipmasked := ip.Mask(net.CIDRMask(l.IPClasses[class][i], total))
	return *(*[16]byte)(ipmasked.To16())
}
