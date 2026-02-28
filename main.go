package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
)

var (
	OpenAIAPIKey = os.Getenv("OPENAI_API_KEY")
	ModelName 	 = os.Getenv("MODEL_NAME")
	VoiceName    = os.Getenv("VOICE_NAME")
)

const (
	DefaultModelName = "gpt-4o-realtime-preview"
	DefaultVoiceName = "marin"
)

const ProsodyInstructions = `Говори естественным женским голосом: светлый, тёплый, без басовой окраски.
Темп средний+, фразы короче; микро-паузы ~200–250 мс между интонационными группами.
Дружелюбная интонация, ясная артикуляция; избегай монотонности и излишней напевности.`

func init() {
	flag.Parse()

	if ModelName == "" {
		ModelName = DefaultModelName
	}
	if VoiceName == "" {
		VoiceName = DefaultVoiceName
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func main() {
	if OpenAIAPIKey == "" {
		log.Fatal("ERROR: OPENAI_API_KEY environment variable is required")
	}

	log.Printf("Starting AI Voice Assistant with model: %s, voice: %s", ModelName, VoiceName)

	wsURL := "wss://api.openai.com/v1/realtime?model=" + ModelName

	rt := NewRealtime(OpenAIAPIKey, wsURL)
	if err := rt.start(); err != nil {
		log.Fatalf("Failed to start realtime system: %v", err)
	}

	log.Println("AI Voice Assistant started. Speak to begin conversation...")
	log.Println("Press Ctrl+C to exit.")

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
	log.Println("Shutting down...")
	rt.stop()
}
