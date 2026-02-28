package main

import (
	"encoding/base64"
	"encoding/binary"
	"log"
	"math"
	"sync"
	"time"

	"github.com/gen2brain/malgo"
)

const (
	Rate          = 24000
	SampleBytes   = 2
	Channels      = 1
	ChunkSize     = 720
	SilenceTail   = 0.40
	GraceSilence  = 450.0
	MinVoiceSec   = 0.40
	AntibounceMs  = 180.0
	MinBufferMs   = 200.0
	EnergyThresh  = 0.012
	VadWarmupSec  = 0.6
	RmsThresh     = 0.0008
	SilenceDurMs  = 500.0
	AnySpeechBarge = true
	AnySpeechGain  = 1.5
	AnySpeechMinMs = 250.0
	BargeAlpha     = 1.1
	BargeBeta      = 0.005
	BargeFloorGain = 1.5
	BargeMinHoldMs = 200.0
	BargePlayRmsMin = 0.0008
	HoldDurationMs  = 500.0
)

type AudioIO struct {
	ctrl              *Realtime
	playBuf           []byte
	micCh             chan []byte
	stopCh            chan struct{}
	recording         bool
	utterBuf          []byte
	utterVoiceMs      float64
	utterStart        time.Time
	lastVoiceTime     time.Time
	lastCommitTs      time.Time
	eouCandidateTs    *time.Time
	energyFloor       float64
	warmupDone        bool
	warmupEnergy      []float64
	playRmsEma        float64
	bargeHoldMs       float64
	anySpeechMs       float64
	silenceStart      *time.Time
	responseEnded     bool
	ctx               *malgo.AllocatedContext
	captureDevice     *malgo.Device
	playbackDevice    *malgo.Device
	mutex             sync.Mutex
}

var globalAudioIO *AudioIO

func NewAudioIO(ctrl *Realtime) *AudioIO {
	audioIO := &AudioIO{
		ctrl:         ctrl,
		playBuf:      make([]byte, 0),
		micCh:        make(chan []byte, 10),
		stopCh:       make(chan struct{}),
		energyFloor:  EnergyThresh,
		warmupEnergy: make([]float64, 0),
	}
	globalAudioIO = audioIO
	return audioIO
}

func bytesForMs(ms float64) int {
	return int((ms / 1000.0) * Rate * SampleBytes * Channels)
}

func (a *AudioIO) start() error {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return err
	}
	a.ctx = ctx

	captureConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	captureConfig.Capture.Format = malgo.FormatS16
	captureConfig.Capture.Channels = Channels
	captureConfig.SampleRate = Rate
	captureConfig.PeriodSizeInFrames = ChunkSize
	captureConfig.Periods = 1

	playbackConfig := malgo.DefaultDeviceConfig(malgo.Playback)
	playbackConfig.Playback.Format = malgo.FormatS16
	playbackConfig.Playback.Channels = Channels
	playbackConfig.SampleRate = Rate
	playbackConfig.PeriodSizeInFrames = ChunkSize
	playbackConfig.Periods = 1

	captureDevice, err := malgo.InitDevice(ctx.Context, captureConfig, malgo.DeviceCallbacks{
		Data: inputCallback,
	})
	if err != nil {
		ctx.Uninit()
		return err
	}

	playbackDevice, err := malgo.InitDevice(ctx.Context, playbackConfig, malgo.DeviceCallbacks{
		Data: outputCallback,
	})
	if err != nil {
		captureDevice.Uninit()
		ctx.Uninit()
		return err
	}

	a.captureDevice = captureDevice
	a.playbackDevice = playbackDevice

	if err := captureDevice.Start(); err != nil {
		playbackDevice.Uninit()
		captureDevice.Uninit()
		ctx.Uninit()
		return err
	}

	if err := playbackDevice.Start(); err != nil {
		captureDevice.Stop()
		playbackDevice.Uninit()
		captureDevice.Uninit()
		ctx.Uninit()
		return err
	}

	log.Println("Audio devices started")
	return nil
}

func inputCallback(pOutput, pInput []byte, frameCount uint32) {
	if globalAudioIO == nil {
		return
	}

	sampleCount := int(frameCount)
	data := make([]byte, sampleCount*2)

	inputSamples := make([]int16, sampleCount)
	for i := 0; i < sampleCount; i++ {
		inputSamples[i] = int16(binary.LittleEndian.Uint16(pInput[i*2:]))
	}

	for i, sample := range inputSamples {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(sample))
	}

	select {
	case globalAudioIO.micCh <- data:
	default:
	}
}

func outputCallback(pOutput, pInput []byte, frameCount uint32) {
	if globalAudioIO == nil {
		return
	}

	globalAudioIO.mutex.Lock()
	defer globalAudioIO.mutex.Unlock()

	sampleCount := int(frameCount)
	need := sampleCount * 2

	if len(globalAudioIO.playBuf) >= need {
		for i := 0; i < sampleCount; i++ {
			sample := int16(binary.LittleEndian.Uint16(globalAudioIO.playBuf[i*2:]))
			binary.LittleEndian.PutUint16(pOutput[i*2:], uint16(sample))
		}
		globalAudioIO.playBuf = globalAudioIO.playBuf[need:]
	} else {
		for i := 0; i < len(pOutput); i++ {
			pOutput[i] = 0
		}
		globalAudioIO.playBuf = globalAudioIO.playBuf[:0]
		globalAudioIO.playRmsEma *= 0.95
	}

	if sampleCount > 0 {
		sum := 0.0
		for i := 0; i < sampleCount; i++ {
			sample := int16(binary.LittleEndian.Uint16(pOutput[i*2:]))
			sampleFloat := float64(sample) / 32768.0
			sum += sampleFloat * sampleFloat
		}
		rms := math.Sqrt(sum / float64(sampleCount))
		globalAudioIO.playRmsEma = globalAudioIO.playRmsEma*0.80 + rms*0.20
	}
}

func (a *AudioIO) loop() {
	chunkMs := (float64(ChunkSize) / Rate) * 1000.0

	for {
		select {
		case <-a.stopCh:
			return
		case data := <-a.micCh:
			a.processAudioChunk(data, chunkMs)
		case <-time.After(100 * time.Millisecond):
			a.checkEOU()
			a.checkSilence()
		}
	}
}

func (a *AudioIO) processAudioChunk(data []byte, chunkMs float64) {
	samples := make([]float64, len(data)/2)
	for i := 0; i < len(samples); i++ {
		sample := int16(binary.LittleEndian.Uint16(data[i*2:]))
		samples[i] = float64(sample) / 32768.0
	}

	energy := 0.0
	for _, sample := range samples {
		energy += math.Abs(sample)
	}
	energy /= float64(len(samples))

	now := time.Now()
	if energy >= a.energyFloor {
		a.lastVoiceTime = now
	}

	if !a.warmupDone {
		a.warmupEnergy = append(a.warmupEnergy, energy)
		totalSamples := len(a.warmupEnergy) * ChunkSize
		if float64(totalSamples)/Rate >= VadWarmupSec {
			median := a.median(a.warmupEnergy)
			a.energyFloor = math.Max(EnergyThresh, median*2.0)
			a.warmupDone = true
			log.Printf("[VAD] Floor set to %.4f", a.energyFloor)
		}
	}

	voiced := energy >= a.energyFloor

	if a.ctrl.hold {
		a.resetRecording()
		return
	}

	if a.ctrl.responding || len(a.playBuf) > 0 {
		if AnySpeechBarge && voiced && (energy >= a.energyFloor*AnySpeechGain) {
			a.anySpeechMs += chunkMs
		} else {
			a.anySpeechMs = 0.0
		}

		cmpTrigger := (a.playRmsEma >= BargePlayRmsMin) && (energy >= (a.playRmsEma*BargeAlpha + BargeBeta))
		floorTrigger := energy >= (a.energyFloor * BargeFloorGain)

		if (a.anySpeechMs >= AnySpeechMinMs) || cmpTrigger || floorTrigger {
			a.bargeHoldMs += chunkMs
			if a.bargeHoldMs >= BargeMinHoldMs {
				a.ctrl.onBargeIn()
				a.bargeHoldMs = 0.0
				a.anySpeechMs = 0.0
				a.resetRecording()
				return
			}
		} else {
			a.bargeHoldMs = 0.0
		}
	}

	if !a.recording && voiced {
		a.recording = true
		a.utterBuf = make([]byte, 0)
		a.utterVoiceMs = 0.0
		a.utterStart = now
		a.eouCandidateTs = nil
	}

	if a.recording {
		a.utterBuf = append(a.utterBuf, data...)
		if voiced {
			a.utterVoiceMs += chunkMs
		}
	}

	a.checkEOU()
	a.checkSilence()
}

func (a *AudioIO) checkEOU() {
	if !a.recording {
		return
	}

	tail := time.Since(a.lastVoiceTime).Seconds()
	longEnough := (a.utterVoiceMs >= MinVoiceSec*1000.0)

	if a.eouCandidateTs != nil {
		if tail < SilenceTail {
			a.eouCandidateTs = nil
			return
		}
		if time.Since(*a.eouCandidateTs).Milliseconds() >= int64(GraceSilence) {
			a.commitAtomic()
			a.resetRecording()
			a.eouCandidateTs = nil
		}
		return
	}

	if longEnough && tail >= SilenceTail {
		ts := time.Now()
		a.eouCandidateTs = &ts
	}
}

func (a *AudioIO) checkSilence() {
	if !a.ctrl.responding && !a.responseEnded {
		return
	}

	now := time.Now()
	if a.playRmsEma < RmsThresh {
		if a.silenceStart == nil {
			ts := now
			a.silenceStart = &ts
		} else if now.Sub(*a.silenceStart).Milliseconds() >= int64(SilenceDurMs) {
			if a.responseEnded && !a.ctrl.responding {
				a.ctrl.writeState(false)
				a.silenceStart = nil
			}
		}
	} else {
		a.silenceStart = nil
		if a.ctrl.responding || len(a.playBuf) > 0 {
			a.ctrl.writeState(true)
		}
	}
}

func (a *AudioIO) commitAtomic() {
	if a.ctrl.hold {
		a.resetRecording()
		return
	}

	if time.Since(a.lastCommitTs).Milliseconds() < int64(AntibounceMs) {
		return
	}

	minCommitBytes := bytesForMs(MinBufferMs)
	if len(a.utterBuf) < minCommitBytes {
		return
	}

	audioCopy := make([]byte, len(a.utterBuf))
	copy(audioCopy, a.utterBuf)

	bufLen := len(audioCopy)

	a.ctrl.txnLock.Lock()
	if a.ctrl.inflightCommit || a.ctrl.inputCleared {
		a.ctrl.txnLock.Unlock()
		return
	}

	txn := a.ctrl.nextTxnID()
	a.ctrl.inflightCommit = true
	a.ctrl.activeTxnID = &txn
	a.ctrl.inputCleared = false

	a.ctrl.txnLock.Unlock()

	if len(audioCopy) > 0 {
		b64 := base64.StdEncoding.EncodeToString(audioCopy)
		a.ctrl.sock.Send(map[string]interface{}{
			"type":  "input_audio_buffer.append",
			"audio": b64,
		})
		a.ctrl.sock.Send(map[string]interface{}{
			"type": "input_audio_buffer.commit",
		})

		ms := (float64(bufLen) / (Rate * SampleBytes * Channels)) * 1000.0
		log.Printf("Commit[txn=%d]: %d bytes (~%.0f ms)", txn, bufLen, ms)
	}

	a.lastCommitTs = time.Now()
}

func (a *AudioIO) resetRecording() {
	a.recording = false
	a.utterBuf = a.utterBuf[:0]
	a.utterVoiceMs = 0.0
	a.utterStart = time.Time{}
	a.eouCandidateTs = nil
}

func (a *AudioIO) pushBack(audioBytes []byte) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.playBuf = append(a.playBuf, audioBytes...)
}

func (a *AudioIO) cutOutput() {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.playBuf = a.playBuf[:0]
	a.playRmsEma *= 0.2
}

func (a *AudioIO) median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	sorted := make([]float64, len(values))
	copy(sorted, values)

	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	n := len(sorted)
	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[n/2]
}

// stop stops audio processing
func (a *AudioIO) stop() {
	close(a.stopCh)

	if a.captureDevice != nil {
		a.captureDevice.Stop()
		a.captureDevice.Uninit()
	}
	if a.playbackDevice != nil {
		a.playbackDevice.Stop()
		a.playbackDevice.Uninit()
	}
	if a.ctx != nil {
		a.ctx.Uninit()
	}

	log.Println("AudioIO stopped")
}
