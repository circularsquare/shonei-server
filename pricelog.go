package main

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Price logging — periodic best bid/ask snapshots, kept in memory and
// persisted to disk so history survives a server restart. Clients fetch the
// history via the price_history_query message and plot it as a price graph.
// ---------------------------------------------------------------------------

const (
	// PriceLogInterval is how often a bid/ask snapshot is taken for every item.
	// Bump to 5*time.Minute for long test sessions.
	PriceLogInterval = 1 * time.Minute

	// PriceLogMaxSamples caps the per-item ring buffer (10080 = 7 days at 1/min).
	// Seven days so the week view has data; queries downsample this one stream.
	PriceLogMaxSamples = 10080

	// PriceLogFile is the on-disk store, written next to traders.json.
	PriceLogFile = "pricelog.json"
)

// PriceSample is one bid/ask snapshot. Prices are in fen. A side is 0 when no
// order rests on it — real prices are always > 0 fen, so 0 is a safe "absent"
// sentinel.
type PriceSample struct {
	T   int64 `json:"t"`   // unix seconds
	Bid int   `json:"bid"` // best bid price, fen; 0 if no bids
	Ask int   `json:"ask"` // best ask price, fen; 0 if no asks
}

// PriceHistory is the in-memory store of per-item sample ring buffers. Its
// mutex is independent of Exchange.mu — the two are never held together.
type PriceHistory struct {
	mu   sync.Mutex
	data map[string][]PriceSample
}

func newPriceHistory() *PriceHistory {
	return &PriceHistory{data: make(map[string][]PriceSample)}
}

// append adds a sample for one item, trimming the buffer to PriceLogMaxSamples.
func (h *PriceHistory) append(item string, s PriceSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	buf := append(h.data[item], s)
	if len(buf) > PriceLogMaxSamples {
		buf = buf[len(buf)-PriceLogMaxSamples:]
	}
	h.data[item] = buf
}

// snapshot returns a copy of one item's history, safe to marshal without the
// lock. Always returns a non-nil slice so the wire payload is [] and not null.
func (h *PriceHistory) snapshot(item string) []PriceSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	src := h.data[item]
	out := make([]PriceSample, len(src))
	copy(out, src)
	return out
}

// downsample reduces a chronological sample list to one point per bucketSec
// time bucket, keeping the first sample of each non-empty bucket. Only samples
// in [start, now] are kept. Each emitted sample's T is snapped to its bucket
// start, so the client sees evenly-spaced buckets. Always returns non-nil.
func downsample(samples []PriceSample, start, now, bucketSec int64) []PriceSample {
	out := make([]PriceSample, 0)
	if bucketSec <= 0 {
		return out
	}
	var lastBucket int64 = -1
	for _, s := range samples {
		if s.T < start || s.T > now {
			continue
		}
		b := s.T / bucketSec
		if b != lastBucket {
			out = append(out, PriceSample{T: b * bucketSec, Bid: s.Bid, Ask: s.Ask})
			lastBucket = b
		}
	}
	return out
}

// save writes the whole store to disk atomically (temp file + rename). The ring
// cap bounds the file size, so a full overwrite each tick stays cheap.
func (h *PriceHistory) save() {
	h.mu.Lock()
	data, err := json.Marshal(h.data)
	h.mu.Unlock()
	if err != nil {
		log.Printf("[pricelog] marshal failed: %v", err)
		return
	}
	tmp := PriceLogFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("[pricelog] write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, PriceLogFile); err != nil {
		log.Printf("[pricelog] rename failed: %v", err)
	}
}

// loadPriceHistory reads pricelog.json at startup. A missing file is fine — it
// just yields an empty store. Each buffer is trimmed in case the cap shrank.
func loadPriceHistory() *PriceHistory {
	h := newPriceHistory()
	data, err := os.ReadFile(PriceLogFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[pricelog] read failed: %v", err)
		}
		return h
	}
	if err := json.Unmarshal(data, &h.data); err != nil {
		log.Printf("[pricelog] parse failed, starting empty: %v", err)
		h.data = make(map[string][]PriceSample)
		return h
	}
	for item, buf := range h.data {
		if len(buf) > PriceLogMaxSamples {
			h.data[item] = buf[len(buf)-PriceLogMaxSamples:]
		}
	}
	log.Printf("[pricelog] loaded history for %d items", len(h.data))
	return h
}

// logSnapshot takes one bid/ask snapshot of every book, appends it to the
// history store, and persists to disk. Each sample carries a wall-clock unix
// timestamp, so a server restart after downtime leaves a real time gap in the
// data — the client renders that gap rather than interpolating across it.
func logSnapshot(h *Hub) {
	now := time.Now().Unix()

	// Snapshot best bid/ask for every book while holding exchange.mu, then
	// release it before touching priceHistory — the two locks are always
	// disjoint, so there is no lock-ordering hazard.
	type itemSample struct {
		item string
		s    PriceSample
	}
	var samples []itemSample
	h.exchange.mu.Lock()
	for item, book := range h.exchange.books {
		var bid, ask int
		if len(book.Buys) > 0 {
			bid = book.Buys[0].Price // best bid (sorted high→low)
		}
		if len(book.Sells) > 0 {
			ask = book.Sells[0].Price // best ask (sorted low→high)
		}
		samples = append(samples, itemSample{item, PriceSample{T: now, Bid: bid, Ask: ask}})
	}
	h.exchange.mu.Unlock()

	for _, is := range samples {
		h.priceHistory.append(is.item, is.s)
	}
	h.priceHistory.save()
	log.Printf("[pricelog] logged %d items", len(samples))
}

// startPriceLogging snapshots every book once immediately (so even a sub-interval
// dev session records a sample), then on every PriceLogInterval. Modeled on
// DynamicTrader.startFarming in bots.go.
//
// Subsequent snapshots are aligned to the wall clock rather than to server
// start time: it sleeps to the next PriceLogInterval boundary before starting
// the ticker, so a 1-min interval logs near :00 of each minute regardless of
// when the server came up. Keeps sample timestamps tidy across restarts.
//
// The same ticker also persists NPC trader stock (saveTraderStock) — it shares
// the logging cadence, so a restart resumes from roughly the last logged state.
func startPriceLogging(h *Hub) {
	go func() {
		logSnapshot(h)    // capture startup prices right away
		saveTraderStock() // and persist current trader stock

		// Sleep until the next interval boundary so logs land on the clock.
		next := time.Now().Truncate(PriceLogInterval).Add(PriceLogInterval)
		time.Sleep(time.Until(next))

		logSnapshot(h)
		saveTraderStock()

		ticker := time.NewTicker(PriceLogInterval)
		defer ticker.Stop()
		for range ticker.C {
			logSnapshot(h)
			saveTraderStock()
		}
	}()
}
