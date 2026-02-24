package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sort"
	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// Message types - these are the "verbs" clients and server speak to each other
// ---------------------------------------------------------------------------

// Envelope wraps every message with a type tag so we know how to parse it.
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}
type ChatMessage struct {
	From string `json:"from"`
	Text string `json:"text"`
}
type OrderMessage struct {
    From     string `json:"from"`
    Item     string `json:"item"`
    Side     string `json:"side"`     // "b" or "s"
    Price    int    `json:"price"`
    Quantity int    `json:"quantity"`
}


// ---------------------------------------------------------------------------
// Client - one per WebSocket connection
// ---------------------------------------------------------------------------

type Client struct {
	conn *websocket.Conn
	name string
	send chan []byte // outbound message queue
}

// ---------------------------------------------------------------------------
// Hub - manages all connected clients and broadcasts
// ---------------------------------------------------------------------------

type Hub struct {
	mu         	sync.RWMutex
	clients    	map[*Client]bool // this is how to say set of clients
	broadcast  	chan []byte
	register   	chan *Client
	unregister 	chan *Client
	exchange 	*Exchange
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		exchange: 	newExchange(),
	}
}

// run is the Hub's main loop. It runs in its own goroutine.
// This pattern (select over channels) is idiomatic Go concurrency.
func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:	
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			log.Printf("Client connected: %s (%d total)", client.name, len(h.clients))

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			h.mu.Unlock()
			log.Printf("Client disconnected: %s (%d total)", client.name, len(h.clients))

		case msg := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.send <- msg:
				default:
					// Client's send buffer is full; drop them.
					close(client.send)
					delete(h.clients, client)
				}
			}
			h.mu.RUnlock()
		}
	}
}

// ---------------------------------------------------------------------------
// Order book stuff
// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------
type Order struct {
	From     string `json:"from"`
	Side     string `json:"side"`
	Price    int    `json:"price"`
	Quantity int    `json:"quantity"`
}
type Book struct {
	Item   string  `json:"item"`
	Buys   []Order `json:"buys"`  // sorted high to low (best bid first)
	Sells  []Order `json:"sells"` // sorted low to high (best ask first)
}
type Exchange struct { 			// Exchange holds all books, keyed by item name.
	books map[string]*Book
}

func (b *Book) insert (o Order) {
	if o.Side == "b" {
		i:= sort.Search(len(b.Buys), func(i int) bool { // ??
			return b.Buys[i].Price < o.Price // find first price lower than o
		})
		b.Buys = append(b.Buys, Order{})	// lengthen list by one
		copy(b.Buys[i+1:], b.Buys[i:]) 	// shift everything right
		b.Buys[i] = o 					
	} else {
		i := sort.Search(len(b.Sells), func(i int) bool {
			return b.Sells[i].Price > o.Price 
		})
		b.Sells = append(b.Sells, Order{})
		copy(b.Sells[i+1:], b.Sells[i:])
		b.Sells[i] = o
	}
}

func newExchange() *Exchange {
	return &Exchange{books: make(map[string]*Book)} 
}

func (ex *Exchange) getBook(item string) *Book {
	if _, ok := ex.books[item]; !ok {	
		ex.books[item] = &Book{Item: item}
	}
	return ex.books[item]
}
// TODO: placeOrder should be called from run() so that it is serialized.
func (ex *Exchange) placeOrder (item string, o Order) {
	book := ex.getBook(item)
	book.insert(o)
}
func (ex *Exchange) printBook (item string) {
	book := ex.getBook(item)
	fmt.Printf("\n=== ORDER BOOK: %s ===\n", item)
	fmt.Println("  SELLS (asks):")
	// Print sells in reverse so highest price is on top, like a real book
	for i := len(book.Sells) - 1; i >= 0; i-- {
		s := book.Sells[i]
		fmt.Printf("    %s  %d @ %d\n", s.From, s.Quantity, s.Price)
	}
	fmt.Println("  -----------")
	fmt.Println("  BUYS (bids):")
	for _, b := range book.Buys {
		fmt.Printf("    %s  %d @ %d\n", b.From, b.Quantity, b.Price)
	}
	fmt.Printf("========================\n\n")
}




// ---------------------------------------------------------------------------
// Client read/write pumps
// ---------------------------------------------------------------------------

// readPump reads messages from the WebSocket and routes them.
func (c *Client) readPump(h *Hub) {
	defer func() { // runs whenever current function exits (on break)
		h.unregister <- c
		c.conn.Close()
	}()

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			break
		}

		// Parse the envelope
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			log.Printf("Bad message from %s: %v", c.name, err)
			continue
		}

		// Route by type
		switch env.Type {
		case "chat":
			// For now, just broadcast it to everyone
			var chat ChatMessage
			if err := json.Unmarshal(env.Payload, &chat); err != nil {
				log.Printf("Bad chat payload: %v", err)
				continue
			}
			chat.From = c.name
			log.Printf("[chat] %s: %s", chat.From, chat.Text)

			// Re-wrap and broadcast
			payload, _ := json.Marshal(chat)
			outEnv, _ := json.Marshal(Envelope{Type: "chat", Payload: payload})
			h.broadcast <- outEnv
		case "order":
			var order OrderMessage
			if err := json.Unmarshal(env.Payload, &order); err != nil {
				log.Printf("bad order payload : %v", err)
				continue
			}
			order.From = c.name
			log.Printf("[order] %s: %s %s %d @ %d", order.From, order.Side, order.Item, order.Quantity, order.Price)

			h.exchange.placeOrder(order.Item, Order{
				From: 	order.From,
				Side:	order.Side,
				Price: 	order.Price,
				Quantity: order.Quantity,
			})
			h.exchange.printBook(order.Item)

			// rewrap and broadcast
			payload, _ := json.Marshal(order)
			outEnv, _ := json.Marshal(Envelope{Type: "order", Payload: payload})
			h.broadcast <- outEnv
		default:
			log.Printf("Unknown message type from %s: %s", c.name, env.Type)
		}
	}
}

// writePump sends messages from the send channel to the WebSocket.
func (c *Client) writePump() {
	defer c.conn.Close()
	for msg := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			break
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP handler - upgrades HTTP to WebSocket
// ---------------------------------------------------------------------------

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for dev. Lock this down in production!
	},
}

func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	// Get player name from query param: ws://localhost:8080/ws?name=PlayerOne
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "anonymous"
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Upgrade error: %v", err)
		return
	}

	client := &Client{
		conn: conn,
		name: name,
		send: make(chan []byte, 256),
	}

	hub.register <- client // registers new client
	// Each client gets two goroutines: one reading, one writing.
	go client.writePump() // starts new goroutines belonging to the new client
	go client.readPump(hub)
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	hub := newHub()
	go hub.run()

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})

	addr := "127.0.0.1:8080" // the 127.0.0.1 means localhost, will need to change eventually
	fmt.Printf("Shonei Market server starting on %s\n", addr)
	fmt.Println("Connect with: ws://localhost:8080/ws?name=YourName")
	log.Fatal(http.ListenAndServe(addr, nil))
}
