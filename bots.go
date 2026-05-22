package main

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// DynamicTrader is an NPC market participant with internal inventory that
// affects pricing. Stock grows over time (farming) and changes with trades.
//
// Pricing (in fen):
//
//	sellPrice = min(maxPrice, defaultPrice * defaultStock / stock + minPrice)
//	buyPrice  = sellPrice / 4
type DynamicTrader struct {
	mu           sync.Mutex
	name         string
	item         string
	stock        int // current inventory
	defaultStock int // baseline stock level
	defaultPrice int // fen — price component at default stock
	minPrice     int // fen — floor (approached as stock → ∞)
	maxPrice     int // fen — ceiling
	showSize     int // quantity shown on sell/buy orders
	maxStockGain int // max stock gained per tick (at <= eq/2)
	maxStockLoss int // max stock lost per tick (at >= 2*eq), stored as positive
	exchange     *Exchange
	hub          *Hub
}

func newDynamicTrader(name, item string, ex *Exchange, hub *Hub) *DynamicTrader {
	return &DynamicTrader{
		name:         name,
		item:         item,
		stock:        2000,
		defaultStock: 2000,
		defaultPrice: 20, // 0.2 liang
		minPrice:     10, // 0.1 liang
		maxPrice:     60, // 0.6 liang
		showSize:     500,
		exchange:     ex,
		hub:          hub,
	}
}

func (t *DynamicTrader) sellPrice() int {
	if t.stock <= 0 {
		return t.maxPrice
	}
	price := t.defaultPrice*t.defaultStock/t.stock + t.minPrice
	if price > t.maxPrice {
		return t.maxPrice
	}
	return price
}

func (t *DynamicTrader) buyPrice() int {
	return t.sellPrice() / 2
}

// refreshOrders atomically cancels all existing orders and re-places them at
// current prices. Any fills against player standing orders are broadcast.
func (t *DynamicTrader) refreshOrders() {
	sp := t.sellPrice()
	bp := t.buyPrice()

	var orders []Order
	if t.stock >= t.showSize {
		orders = append(orders, Order{
			From:       t.name,
			Side:       "s",
			Price:      sp,
			Quantity:   t.showSize,
			ClientType: "bot",
		})
	}
	orders = append(orders, Order{
		From:       t.name,
		Side:       "b",
		Price:      bp,
		Quantity:   t.showSize,
		ClientType: "bot",
	})

	t.exchange.cancelOrdersForItem(t.name, t.item)
	var allFills []Fill
	for _, o := range orders {
		fills, _ := t.exchange.placeOrder(t.item, o)
		allFills = append(allFills, fills...)
	}
	log.Printf("[bot] %s/%s: stock=%d sp=%d bp=%d placing=%d fills=%d", t.name, t.item, t.stock, sp, bp, len(orders), len(allFills))

	// Broadcast any fills that happened against player standing orders
	if t.hub != nil && len(allFills) > 0 {
		for _, f := range allFills {
			log.Printf("[fill] %s bought %d %s from %s @ %d (bot refresh)", f.Buyer, f.Quantity, f.Item, f.Seller, f.Price)
			payload, _ := json.Marshal(f)
			outEnv, _ := json.Marshal(Envelope{Type: "fill", Payload: payload})
			t.hub.broadcast <- outEnv
		}
		// Broadcast updated book
		book := t.exchange.getBook(t.item)
		bookPayload, _ := json.Marshal(book)
		bookEnv, _ := json.Marshal(Envelope{Type: "market_response", Payload: bookPayload})
		t.hub.broadcast <- bookEnv
	}
}

// onFill adjusts stock when a trade involves this trader, then refreshes orders.
// Item match is required: a nation may run multiple traders (one per item), so
// fan-out from handleFillForTraders hits every trader sharing the nation's name.
// Without the item gate, selling wheat to Fulan would also bump Fulan's soybean,
// ramie, and bamboo stocks.
func (t *DynamicTrader) onFill(f Fill) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if f.Item != t.item {
		return
	}

	if f.Seller == t.name {
		t.stock -= f.Quantity
		if t.stock < 0 {
			t.stock = 0
		}
	} else if f.Buyer == t.name {
		t.stock += f.Quantity
	} else {
		return
	}

	t.refreshOrders()
}

// stockDelta computes how much stock changes per tick based on distance from
// equilibrium (defaultStock).
//
//	stock <= eq/2        → +maxStockGain
//	eq/2 < stock < eq    → +maxStockGain to 0 linearly
//	stock == eq          → 0 (equilibrium)
//	eq < stock < 2*eq    → 0 to -maxStockLoss linearly
//	stock >= 2*eq        → -maxStockLoss
func (t *DynamicTrader) stockDelta() int {
	eq := t.defaultStock
	switch {
	case t.stock <= eq/2:
		return t.maxStockGain
	case t.stock < eq:
		return t.maxStockGain * (eq - t.stock) / (eq / 2)
	case t.stock == eq:
		return 0
	case t.stock < 2*eq:
		return -t.maxStockLoss * (t.stock - eq) / eq
	default:
		return -t.maxStockLoss
	}
}

// startFarming runs a tick every 10 seconds that adjusts stock toward equilibrium.
func (t *DynamicTrader) startFarming() {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			t.mu.Lock()
			delta := t.stockDelta()
			if delta != 0 {
				log.Printf("[farming] %s/%s tick: stock=%d delta=%d", t.name, t.item, t.stock, delta)
				t.stock += delta
				if t.stock < 0 {
					t.stock = 0
				}
				t.refreshOrders()
			}
			t.mu.Unlock()
		}
	}()
}

// ── Config & registry ────────────────────────────────────────────────────

type TraderConfig struct {
	Name         string `json:"name"`
	Item         string `json:"item"`
	Stock        int    `json:"stock"`
	DefaultStock int    `json:"default_stock"`
	DefaultPrice int    `json:"default_price"`
	MinPrice     int    `json:"min_price"`
	MaxPrice     int    `json:"max_price"`
	ShowSize     int    `json:"show_size"`
	MaxStockGain int    `json:"max_stock_gain"`
	MaxStockLoss int    `json:"max_stock_loss"`
}

var dynamicTraders []*DynamicTrader

func initDynamicTraders(ex *Exchange, hub *Hub) {
	data, err := os.ReadFile("traders.json")
	if err != nil {
		log.Fatalf("failed to read traders.json: %v", err)
	}
	var configs []TraderConfig
	if err := json.Unmarshal(data, &configs); err != nil {
		log.Fatalf("failed to parse traders.json: %v", err)
	}

	// Saved stock from a previous run wins over the traders.json baseline, so a
	// restart resumes prices where they left off. traders.json `stock` is only
	// the cold-start value (no save file, or a trader newly added to config).
	savedStock := loadTraderStock()

	for _, cfg := range configs {
		stock := cfg.Stock
		if s, ok := savedStock[traderStockKey(cfg.Name, cfg.Item)]; ok {
			stock = s
		}
		t := &DynamicTrader{
			name:         cfg.Name,
			item:         cfg.Item,
			stock:        stock,
			defaultStock: cfg.DefaultStock,
			defaultPrice: cfg.DefaultPrice,
			minPrice:     cfg.MinPrice,
			maxPrice:     cfg.MaxPrice,
			showSize:     cfg.ShowSize,
			maxStockGain: cfg.MaxStockGain,
			maxStockLoss: cfg.MaxStockLoss,
			exchange:     ex,
			hub:          hub,
		}
		t.refreshOrders()
		t.startFarming()
		dynamicTraders = append(dynamicTraders, t)
		log.Printf("[init] loaded trader %s (%s)", cfg.Name, cfg.Item)
	}
}

type TraderStockInfo struct {
	Name      string `json:"name"`
	Item      string `json:"item"`
	Stock     int    `json:"stock"`
	SellPrice int    `json:"sell_price"`
	BuyPrice  int    `json:"buy_price"`
}

func getTraderStock(name string) *TraderStockInfo {
	for _, t := range dynamicTraders {
		if t.name == name {
			t.mu.Lock()
			defer t.mu.Unlock()
			return &TraderStockInfo{
				Name:      t.name,
				Item:      t.item,
				Stock:     t.stock,
				SellPrice: t.sellPrice(),
				BuyPrice:  t.buyPrice(),
			}
		}
	}
	return nil
}

func handleFillForTraders(f Fill) {
	for _, t := range dynamicTraders {
		t.onFill(f)
	}
}

// ── Stock persistence ────────────────────────────────────────────────────
//
// Trader stock drifts continuously (farming + fills), and stock alone
// determines price. Persisting it lets a server restart resume from the last
// logged stock instead of snapping every trader back to its traders.json
// baseline. Saved on the price-logging cadence — see startPriceLogging.

// TraderStockFile is the on-disk store, written next to traders.json.
const TraderStockFile = "traderstock.json"

// TraderStockEntry is one trader's persisted stock. Name and Item together
// identify a trader: a nation may run several traders, one per item.
type TraderStockEntry struct {
	Name  string `json:"name"`
	Item  string `json:"item"`
	Stock int    `json:"stock"`
}

// traderStockKey is the lookup key for the loaded-stock map. The NUL separator
// can't collide with any nation or item name.
func traderStockKey(name, item string) string {
	return name + "\x00" + item
}

// saveTraderStock writes every trader's current stock to disk atomically
// (temp file + rename). Each trader's mutex is held only for the stock read.
func saveTraderStock() {
	entries := make([]TraderStockEntry, 0, len(dynamicTraders))
	for _, t := range dynamicTraders {
		t.mu.Lock()
		entries = append(entries, TraderStockEntry{Name: t.name, Item: t.item, Stock: t.stock})
		t.mu.Unlock()
	}
	data, err := json.Marshal(entries)
	if err != nil {
		log.Printf("[traderstock] marshal failed: %v", err)
		return
	}
	tmp := TraderStockFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("[traderstock] write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, TraderStockFile); err != nil {
		log.Printf("[traderstock] rename failed: %v", err)
	}
}

// loadTraderStock reads traderstock.json into a name+item → stock lookup map.
// A missing file yields an empty map — every trader then falls back to its
// traders.json baseline. A parse error does the same, after logging.
func loadTraderStock() map[string]int {
	out := make(map[string]int)
	data, err := os.ReadFile(TraderStockFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[traderstock] read failed: %v", err)
		}
		return out
	}
	var entries []TraderStockEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("[traderstock] parse failed, using baselines: %v", err)
		return out
	}
	for _, e := range entries {
		out[traderStockKey(e.Name, e.Item)] = e.Stock
	}
	log.Printf("[traderstock] loaded stock for %d traders", len(out))
	return out
}
