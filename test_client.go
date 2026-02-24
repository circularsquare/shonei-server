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

	"github.com/gorilla/websocket"
)

type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type ChatMessage struct {
	From string `json:"from"`
	Text string `json:"text"`
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

		payload, _ := json.Marshal(ChatMessage{Text: text})
		env, _ := json.Marshal(Envelope{Type: "chat", Payload: payload})
		if err := conn.WriteMessage(websocket.TextMessage, env); err != nil {
			log.Fatal("Send error:", err)
		}
		fmt.Print("> ")
	}

	// Graceful shutdown on Ctrl+C
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	<-interrupt
}
