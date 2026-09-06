package tui

// voice.go implements minimal voice-mode for metis: shell-out audio
// recording + OpenAI Whisper transcription + text injection into the
// chat input. Reference: claude-code's `voice/voiceModeEnabled.ts`
// architecture (gated by feature flag + auth check) but with a much
// smaller surface — claude-code uses Anthropic's voice_stream
// endpoint (private), we use the publicly-available Whisper API.
//
// Why shell-out for recording: pure-Go audio capture requires CGO
// (PortAudio bindings) which complicates cross-compilation. Every
// modern OS ships an audio recording CLI:
//   - macOS:   sox / rec (brew) → fallback ffmpeg with avfoundation
//   - Linux:   arecord (ALSA) / ffmpeg with pulse
//   - Windows: ffmpeg with dshow
//
// Toggle UX: `/voice` starts a 30s recording (auto-stops on timeout
// or explicit /voice stop). On stop we POST the WAV to
// api.openai.com/v1/audio/transcriptions and inject the response text
// into the input — exactly what claude-code's PromptInputFooter does
// when its private voice_stream returns.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/auth"
)

// voiceMaxRecord caps /voice recording duration. Mutable so
// config.UI.VoiceMaxRecord() can override at startup via
// SetVoiceMaxRecord (2026-05-22).
var voiceMaxRecord = 30 * time.Second

// SetVoiceMaxRecord overrides the auto-stop window for /voice
// recording. Called once at startup from cmd/metis/main.go.
func SetVoiceMaxRecord(d time.Duration) {
	if d > 0 {
		voiceMaxRecord = d
	}
}

var (
	voiceMu       sync.Mutex
	voiceCmd      *exec.Cmd
	voiceFile     string
	voiceCallback func(transcript string, err error) // set by chat surface
)

// voiceActive reports whether a recording is currently in progress.
// Used by the status bar to show a "● rec" indicator.
func voiceActive() bool {
	voiceMu.Lock()
	defer voiceMu.Unlock()
	return voiceCmd != nil
}

// startVoiceRecording spawns the OS-appropriate recorder writing PCM
// to a temp WAV. Returns immediately (recording happens in the
// subprocess); call stopVoiceRecording to halt + transcribe.
func startVoiceRecording() error {
	voiceMu.Lock()
	defer voiceMu.Unlock()
	if voiceCmd != nil {
		return fmt.Errorf("recording already active")
	}

	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("metis-voice-%d.wav", time.Now().UnixNano()))
	cmd, err := pickRecorder(tmp)
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	voiceCmd = cmd
	voiceFile = tmp

	// Auto-stop after voiceMaxRecord. Caller's callback fires with
	// the transcribed text. We run this off-thread so the chat
	// surface keeps redrawing during the recording.
	go func() {
		time.Sleep(voiceMaxRecord)
		voiceMu.Lock()
		stillRunning := voiceCmd != nil
		voiceMu.Unlock()
		if stillRunning {
			text, err := stopVoiceRecording()
			if voiceCallback != nil {
				voiceCallback(text, err)
			}
		}
	}()
	return nil
}

// stopVoiceRecording halts the recorder and transcribes the captured
// audio via Whisper. Returns the transcribed text or an error. Safe
// to call when no recording is active (returns "no recording").
func stopVoiceRecording() (string, error) {
	voiceMu.Lock()
	cmd := voiceCmd
	file := voiceFile
	voiceCmd = nil
	voiceFile = ""
	voiceMu.Unlock()
	if cmd == nil {
		return "", fmt.Errorf("no recording active")
	}
	// SIGINT lets sox/rec/arecord/ffmpeg flush their WAV header
	// before exiting — SIGKILL produces a corrupt file.
	_ = cmd.Process.Signal(os.Interrupt)
	_ = cmd.Wait()

	defer os.Remove(file)
	data, err := os.ReadFile(file)
	if err != nil || len(data) == 0 {
		return "", fmt.Errorf("recording produced no audio")
	}
	return whisperTranscribe(data)
}

// pickRecorder returns the best available OS recorder configured to
// write a 16kHz mono WAV (Whisper's preferred input format).
func pickRecorder(outFile string) (*exec.Cmd, error) {
	switch runtime.GOOS {
	case "darwin":
		// rec is sox-installed-as-rec; prefer over ffmpeg because
		// avfoundation needs explicit device-id flags that vary by
		// hardware. Rec auto-picks the default mic.
		if _, err := exec.LookPath("rec"); err == nil {
			return exec.Command("rec", "-q", "-r", "16000", "-c", "1", outFile), nil
		}
		if _, err := exec.LookPath("sox"); err == nil {
			return exec.Command("sox", "-q", "-d", "-r", "16000", "-c", "1", outFile), nil
		}
		// ffmpeg fallback — assumes default audio device :0
		if _, err := exec.LookPath("ffmpeg"); err == nil {
			return exec.Command("ffmpeg", "-y", "-f", "avfoundation", "-i", ":0", "-ar", "16000", "-ac", "1", outFile), nil
		}
	case "linux":
		if _, err := exec.LookPath("arecord"); err == nil {
			return exec.Command("arecord", "-q", "-r", "16000", "-c", "1", "-f", "S16_LE", outFile), nil
		}
		if _, err := exec.LookPath("ffmpeg"); err == nil {
			return exec.Command("ffmpeg", "-y", "-f", "pulse", "-i", "default", "-ar", "16000", "-ac", "1", outFile), nil
		}
	case "windows":
		if _, err := exec.LookPath("ffmpeg"); err == nil {
			return exec.Command("ffmpeg", "-y", "-f", "dshow", "-i", "audio=Microphone", "-ar", "16000", "-ac", "1", outFile), nil
		}
	}
	return nil, fmt.Errorf("no audio recorder found — install sox/rec (macOS), arecord (linux), or ffmpeg")
}

// whisperTranscribe sends a WAV blob to OpenAI's
// /v1/audio/transcriptions endpoint and returns the recognized text.
// Uses the OpenAI API key stored under metis's auth file.
func whisperTranscribe(audio []byte) (string, error) {
	apiKey, err := auth.Get("openai")
	if err != nil || apiKey == "" {
		return "", fmt.Errorf("openai api key required for voice transcription — run `metis login openai`")
	}

	body := &bytes.Buffer{}
	mp := multipart.NewWriter(body)
	w, err := mp.CreateFormFile("file", "recording.wav")
	if err != nil {
		return "", err
	}
	if _, err := w.Write(audio); err != nil {
		return "", err
	}
	if err := mp.WriteField("model", "whisper-1"); err != nil {
		return "", err
	}
	if err := mp.Close(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/audio/transcriptions", body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", mp.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("whisper api: %s", string(respBody))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", err
	}
	return out.Text, nil
}
