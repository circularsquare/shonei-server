package main

// A tiny CLI client for testing the server.
// Run in a separate terminal:  go run test_client.go -name=Mouse1

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
    "strings"

	"github.com/gorilla/websocket"
)

// this has to match the structs in main.go
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

func main() {
	name := flag.String("name", "Mouse", "Your player name")
	addr := flag.String("addr", "localhost:8080", "Server address")
	flag.Parse()

	url := fmt.Sprintf("ws://%s/ws?name=%s", *addr, *name)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		log.Fatal("Connection failed:", err)
	}
	defer conn.Close()
	fmt.Printf("Connected to %s as %s\n", url, *name)
	fmt.Println("Type messages and press Enter. Ctrl+C to quit.")

	// Print incoming messages
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				log.Println("Disconnected:", err)
				os.Exit(0)
			}
			var env Envelope
			json.Unmarshal(raw, &env)

			switch env.Type {
			case "chat":
				var chat ChatMessage
				json.Unmarshal(env.Payload, &chat)
				fmt.Printf("\n  [%s]: %s\n> ", chat.From, chat.Text)
			case "order":
				var order OrderMessage
				json.Unmarshal(env.Payload, &order)
				fmt.Printf("\n  [%s]: %s : %d \n> ", order.From, order.Item, order.Price)
			default:
				fmt.Printf("\n  (unknown type: %s) %s\n> ", env.Type, string(env.Payload))
			}
		}
	}()

	// Read stdin and send as chat messages
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")
	for scanner.Scan() {
		text := scanner.Text()
		if text == "" {
			fmt.Print("> ")
			continue
		}

		var env []byte 

		if strings.HasPrefix(text, "/order") {
			// Parse: /order b wood 50 10  (side item price quantity)
			parts := strings.Fields(text)
			if len(parts) != 5 {
				fmt.Println("  Usage: /order <b|s> <item> <price> <quantity>")
            	fmt.Print("> ")
				continue
			}
			price, err1 := strconv.Atoi(parts[3])
			qty, err2 := strconv.Atoi(parts[4])
			if err1 != nil || err2 != nil {
				fmt.Println("  Price and quantity must be numbers")
				fmt.Print("> ")
				continue
			}
			payload, _ := json.Marshal(OrderMessage {
				Item: 		parts[2],
				Side: 		parts[1],
				Price: 		price,
				Quantity: 	qty,
			})
			env, _ = json.Marshal(Envelope{Type: "order", Payload: payload})
			if err := conn.WriteMessage(websocket.TextMessage, env); err != nil {
				log.Fatal("Send error:", err)
			}
			fmt.Print("> ")
		} else {
			payload, _ := json.Marshal(ChatMessage{Text: text})
			env, _ = json.Marshal(Envelope{Type: "chat", Payload: payload})
			if err := conn.WriteMessage(websocket.TextMessage, env); err != nil {
				log.Fatal("Send error:", err)
			}
			fmt.Print("> ")
		}
		
		
	}

	// Graceful shutdown on Ctrl+C
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	<-interrupt
}
