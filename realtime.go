package main

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

const StateFile = "state.json"

type Realtime struct {
	sock             	*Socket
	sentUpdate       	bool
	responding       	bool
	activeResponseID 	*string
	ignoreIDs        	map[string]bool
	hold             	bool
	holdTimer        	*time.Timer
	inputCleared     	bool
	lastCommitLen    	int
	txnLock          	sync.Mutex
	txnGen           	int
	inflightCommit   	bool
	inflightCreate   	bool
	activeTxnID      	*int
	latestCommittedTxn  *int
	lastCreatedForTxn 	*int
	pendingCreate    	bool
	audio            	*AudioIO
}

func NewRealtime(apiKey, wsURL string) *Realtime {
	rt := &Realtime{
		sock:      NewSocket(apiKey, wsURL),
		ignoreIDs: make(map[string]bool),
		audio:     NewAudioIO(nil),
	}
	rt.audio.ctrl = rt
	rt.writeState(false)
	return rt
}

func (rt *Realtime) writeState(isTalking bool) {
	maxRMS := 0.01
	intensity := 0.0
	if isTalking {
		intensity = min(1.0, max(0.0, rt.audio.playRmsEma/maxRMS))
	}

	state := map[string]interface{}{
		"is_talking": isTalking,
		"intensity":  intensity,
	}

	data, err := json.Marshal(state)
	if err != nil {
		log.Printf("Failed to marshal state: %v", err)
		return
	}

	if err := os.WriteFile(StateFile, data, 0644); err != nil {
		log.Printf("Failed to write state file: %v", err)
	}
}

func (rt *Realtime) nextTxnID() int {
	rt.txnGen++
	return rt.txnGen
}

func getResponseID(msg map[string]interface{}) *string {
	if resp, ok := msg["response"].(map[string]interface{}); ok {
		if rid, ok := resp["id"].(string); ok {
			return &rid
		}
	}
	if rid, ok := msg["response_id"].(string); ok {
		return &rid
	}
	return nil
}

func (rt *Realtime) onBargeIn() {
	rt.txnLock.Lock()
	defer rt.txnLock.Unlock()

	rt.audio.cutOutput()
	rid := rt.activeResponseID
	if rid != nil {
		rt.sock.Send(map[string]interface{}{
			"type":         "response.cancel",
			"response_id": *rid,
		})
		log.Printf("[CANCEL sent] rid=%s", *rid)
		rt.ignoreIDs[*rid] = true
	}

	rt.responding = false
	rt.activeResponseID = nil
	rt.audio.responseEnded.Store(false)
	rt.writeState(false)
	rt.sock.Send(map[string]interface{}{
		"type": "input_audio_buffer.clear",
	})
	rt.inputCleared = true
	rt.inflightCommit = false
	rt.inflightCreate = false
	rt.activeTxnID = nil
	rt.pendingCreate = false
	rt.lastCommitLen = 0
	rt.setHold(true)
	log.Println("[HOLD ON] (0.5s)")

	if rt.holdTimer != nil {
		rt.holdTimer.Stop()
	}
	rt.holdTimer = time.AfterFunc(HoldDurationMs*time.Millisecond, rt.releaseHold)
}

func (rt *Realtime) setHold(val bool) {
	rt.hold = val
}

func (rt *Realtime) releaseHold() {
	rt.txnLock.Lock()
	defer rt.txnLock.Unlock()

	if !rt.hold {
		return
	}
	rt.setHold(false)
	rt.inputCleared = false
	log.Println("[HOLD OFF]")
}

func (rt *Realtime) start() error {
	rt.sock.SetMessageHandler(rt.onMessage)

	if err := rt.sock.Connect(); err != nil {
		return err
	}

	if err := rt.audio.start(); err != nil {
		rt.sock.Close()
		return err
	}

	go rt.audio.loop()

	return nil
}

func (rt *Realtime) stop() {
	rt.audio.stop()
	rt.sock.Close()
}

func (rt *Realtime) onMessage(msg map[string]interface{}) {
	typ, ok := msg["type"].(string)
	if !ok {
		return
	}

	switch typ {
	case "session.created":
		if !rt.sentUpdate {
			rt.sentUpdate = true
			rt.sock.Send(map[string]interface{}{
				"type": "session.update",
				"session": map[string]interface{}{
					"modalities":                []string{"audio", "text"},
					"voice":                     VoiceName,
					"instructions":              ProsodyInstructions,
					"input_audio_format":        "pcm16",
					"output_audio_format":       "pcm16",
					"input_audio_transcription": map[string]interface{}{
						"model":    "gpt-4o-mini-transcribe",
						"language": "ru",
					},
				},
			})
			log.Println("Session updated.")
		}

	case "response.created":
		resp, ok := msg["response"].(map[string]interface{})
		if !ok {
			return
		}
		rid, ok := resp["id"].(string)
		if !ok {
			return
		}

		if !rt.ignoreIDs[rid] {
			if rt.activeResponseID == nil {
				rt.activeResponseID = &rid
				rt.responding = true
				rt.audio.responseEnded.Store(false)
				rt.writeState(true)
				log.Printf("[RESP] created id=%s", rid)
			}
		}

	case "response.output_audio.delta", "response.audio.delta":
		if rt.hold {
			return
		}
		rid := getResponseID(msg)
		if rid == nil || rt.activeResponseID == nil || *rid != *rt.activeResponseID || rt.ignoreIDs[*rid] {
			return
		}

		var payload string
		if p, ok := msg["delta"].(string); ok {
			payload = p
		} else if p, ok := msg["audio"].(string); ok {
			payload = p
		} else if p, ok := msg["data"].(string); ok {
			payload = p
		} else if data, ok := msg["data"].([]interface{}); ok {
			// Handle array of bytes
			audioBytes := make([]byte, len(data))
			for i, v := range data {
				if b, ok := v.(float64); ok {
					audioBytes[i] = byte(b)
				}
			}
			rt.audio.pushBack(audioBytes)
			return
		} else {
			return
		}

		if payload != "" {
			audioBytes, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				log.Printf("Failed to decode audio: %v", err)
				return
			}
			rt.audio.pushBack(audioBytes)
		}

	case "response.output_audio_transcript.delta", "response.audio_transcript.delta",
		"response.text.delta", "response.output_text.delta":
		if rt.hold {
			return
		}
		rid := getResponseID(msg)
		if rid == nil || rt.activeResponseID == nil || *rid != *rt.activeResponseID || rt.ignoreIDs[*rid] {
			return
		}

		if delta, ok := msg["delta"].(string); ok && delta != "" {
			print(delta)
		}

	case "response.output_audio_transcript.done", "response.audio_transcript.done",
		"response.output_text.done", "response.text.done":
		if !rt.hold {
			println()
		}

	case "input_audio_buffer.committed":
		rt.txnLock.Lock()
		if !rt.inflightCommit {
			rt.txnLock.Unlock()
			return
		}

		if rt.lastCommitLen == 0 {
			log.Println("[PIPE] committed but no audio sent - ignoring")
			rt.inflightCommit = false
			rt.activeTxnID = nil
			rt.txnLock.Unlock()
			return
		}

		rt.inflightCommit = false
		rt.latestCommittedTxn = rt.activeTxnID
		if rt.latestCommittedTxn != nil {
			log.Printf("[PIPE] committed ack txn=%d", *rt.latestCommittedTxn)
		}

		if rt.responding || rt.hold || rt.inputCleared {
			rt.pendingCreate = true
			log.Println("[PIPE] responding/HOLD/cleared → pending_create=True")
			rt.txnLock.Unlock()
			return
		}

		if rt.lastCreatedForTxn != rt.latestCommittedTxn && !rt.inflightCreate {
			rt.createResponseLocked()
		}
		rt.txnLock.Unlock()

	case "response.done":
		resp, ok := msg["response"].(map[string]interface{})
		if !ok {
			return
		}
		rid, ok := resp["id"].(string)
		if !ok {
			return
		}

		if rt.ignoreIDs[rid] {
			delete(rt.ignoreIDs, rid)
		}
		if rt.activeResponseID != nil && rid == *rt.activeResponseID {
			rt.responding = false
			rt.activeResponseID = nil
			rt.audio.responseEnded.Store(true)
		}

		rt.txnLock.Lock()
		if !rt.inputCleared {
			rt.sock.Send(map[string]interface{}{
				"type": "input_audio_buffer.clear",
			})
		}

		if rt.pendingCreate && !rt.hold && !rt.inputCleared {
			rt.pendingCreate = false
			if rt.latestCommittedTxn != nil &&
				rt.lastCreatedForTxn != rt.latestCommittedTxn &&
				!rt.inflightCreate {
				rt.createResponseLocked()
			}
		}
		rt.txnLock.Unlock()

	case "error":
		err, ok := msg["error"].(map[string]interface{})
		if !ok {
			return
		}

		code, _ := err["code"].(string)
		message, _ := err["message"].(string)

		if code == "input_audio_buffer_commit_empty" {
			log.Printf("Ignored empty commit error: %s", message)
		} else if code == "conversation_already_has_active_response" {
			log.Printf("Ignored duplicate response error: %s", message)
		} else {
			log.Printf("Server error: %v", err)
		}
	}
}

func (rt *Realtime) createResponseLocked() {
	if rt.hold || rt.responding || rt.inputCleared {
		return
	}
	if rt.latestCommittedTxn == nil {
		return
	}

	rt.sock.Send(map[string]interface{}{
		"type": "response.create",
		"response": map[string]interface{}{
			"modalities": []string{"audio", "text"},
			"voice":      VoiceName,
			"instructions": ProsodyInstructions,
		},
	})
	rt.inflightCreate = true
	rt.lastCreatedForTxn = rt.latestCommittedTxn
	if rt.lastCreatedForTxn != nil {
		log.Printf("[RESP] response.create sent for txn=%d", *rt.lastCreatedForTxn)
	}
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
