# Shonei Market Server

A WebSocket server for the Shonei player marketplace, written in Go.

## Setup

### 1. Install Go
Download from https://go.dev/dl/ and follow the instructions for your OS.
Verify with: `go version`

### 2. Initialize and run
```bash
cd shonei-market
go mod tidy          # downloads the gorilla/websocket dependency
go run main.go       # starts the server on :8080
```

### 3. Test it
Open a second terminal:
```bash
go run test_client.go -name=Mouse1
```
Open a third terminal:
```bash
go run test_client.go -name=Mouse2
```
Type in either terminal — messages broadcast to both. You've got a working
WebSocket server!

## Architecture overview

```
┌─────────────┐     WebSocket      ┌─────────────────────────┐
│ Game Client │ ◄────────────────► │       Go Server         │
│ (Godot/JS)  │                    │                         │
└─────────────┘                    │  Hub (goroutine)        │
                                   │    ├─ register chan     │
┌─────────────┐     WebSocket      │    ├─ unregister chan   │
│ Game Client │ ◄────────────────► │    └─ broadcast chan    │
└─────────────┘                    │                         │
                                   │  Per client:            │
                                   │    ├─ readPump (goroutine)  │
                                   │    └─ writePump (goroutine) │
                                   └─────────────────────────┘
```

### Key concepts

**Envelope pattern**: Every message is wrapped in `{"type": "...", "payload": {...}}`.
This lets you add new message types (orders, fills, market data) without changing
the transport layer. Just add a new `case` in the `switch`.

**Hub + Client pattern**: The Hub is the central coordinator. Clients never talk
to each other directly — they send to the Hub, and the Hub broadcasts. This is
the standard Go WebSocket pattern (straight from the gorilla/websocket examples).

**Goroutines**: Each client gets two goroutines (readPump, writePump) plus the
Hub runs in its own goroutine. Go makes this cheap — you can have thousands of
concurrent connections without sweating.

## Next steps (your roadmap)

1. **You are here** → Server accepts connections, echoes messages
2. Add message types: `place_order`, `cancel_order`, `market_data`
3. Build an in-memory order book (price-time priority matching)
4. Broadcast fills and book updates to all clients
5. Connect your game client (Godot WebSocket or JS)
6. Add persistence (SQLite or PostgreSQL)
7. Add NPC orders (server-side bots that post to seed liquidity)
