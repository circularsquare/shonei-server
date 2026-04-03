# Shonei Market Server — Claude Instructions

## Context

This is the Go WebSocket server for the Shonei game. For full game context, read `../shonei/CLAUDE.md` first, then `../shonei/Assets/spec/SPEC.md` for the game spec.

## Architecture

- `main.go` — Hub, Exchange, order book, WebSocket handlers
- `bots.go` — Foreign trader NPCs that seed baseline liquidity
- `client/main.go` — CLI test client

## Conventions

### Fen everywhere
All prices and quantities on the wire and in server code are **fen** (`int`, 100 fen = 1 liang). The Unity client converts liang ↔ fen at the UI boundary. The CLI test client also uses fen directly.

### Server address
The server listens on `127.0.0.1:8082`. The Unity client (`TradingClient.cs`) and `startserver.sh` (in `../shonei/`) both expect this port.

### Envelope pattern
Every WebSocket message is `{"type": "...", "payload": {...}}`. To add a new message type, add a struct, then a `case` in the `readPump` switch.

### Hub + Client pattern
Clients never talk directly — they send to the Hub via channels, and the Hub broadcasts. Each client gets two goroutines (readPump, writePump).

### Dynamic traders (bots.go)
NPC nations with internal inventory that affects pricing. They run server-side with direct Exchange access. Key mechanics:
- **Stock-based pricing**: sell price scales inversely with stock (`min(maxPrice, defaultPrice * defaultStock / stock + minPrice)`), buy price is `sellPrice / 4`.
- **Farming equilibrium**: a ticker adjusts stock toward `defaultStock` — gaining stock when below equilibrium (high prices), losing stock when above (low prices).
- **Order refresh**: after every fill involving a bot or on each farming tick, old orders are cancelled and re-placed at current prices. `showSize` controls the quantity shown on each order.
- **`client_type` field**: every `Order` carries a `client_type` of `"bot"` or `"player"` so clients can distinguish them visually.
