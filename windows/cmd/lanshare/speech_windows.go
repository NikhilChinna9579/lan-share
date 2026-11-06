package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Voice for the laptop's assistant: Windows' own speech recogniser (System.Speech: free,
// offline, nothing to download), in a small PowerShell helper that stays running so
// listening starts at once. It expects LAN Share's command sentences (with the phones'
// names), which makes it accurate, and still takes free speech.

const speechScript = `
[Console]::OutputEncoding = [Text.Encoding]::UTF8
Add-Type -AssemblyName System.Speech
$r = [System.Speech.Recognition.SpeechRecognitionEngine]::InstalledRecognizers() | Where-Object { $_.Culture.Name -like 'en-*' } | Sort-Object { if ($_.Culture.Name -eq 'en-US') { 0 } else { 1 } } | Select-Object -First 1
if (-not $r) { [Console]::Out.WriteLine('ERR no-recognizer'); exit }
$e = New-Object System.Speech.Recognition.SpeechRecognitionEngine($r)
$ch = New-Object System.Speech.Recognition.Choices
$ch.Add([string[]](Get-Content -LiteralPath $args[0] -Encoding UTF8))
$gb = New-Object System.Speech.Recognition.GrammarBuilder($ch)
$gb.Culture = $r.Culture
$e.LoadGrammar((New-Object System.Speech.Recognition.Grammar($gb)))
try { $e.LoadGrammar((New-Object System.Speech.Recognition.DictationGrammar)) } catch {}
$wav = $env:LANSHARE_SPEECH_WAV
try { if ($wav) { $e.SetInputToWaveFile($wav) } else { $e.SetInputToDefaultAudioDevice() } } catch { [Console]::Out.WriteLine('ERR no-mic'); exit }
$e.InitialSilenceTimeout = [TimeSpan]::FromSeconds(6)
$e.EndSilenceTimeout = [TimeSpan]::FromMilliseconds(800)
$e.BabbleTimeout = [TimeSpan]::FromSeconds(10)
[Console]::Out.WriteLine('READY ' + $r.Culture.Name)
while ($null -ne ($line = [Console]::In.ReadLine())) {
  if ($line -ne 'listen') { continue }
  if ($wav) { $e.SetInputToWaveFile($wav) }
  $res = $null
  try { $res = $e.Recognize([TimeSpan]::FromSeconds(9)) } catch {}
  if ($res) { [Console]::Out.WriteLine('HEARD' + [char]9 + $res.Confidence + [char]9 + $res.Text) } else { [Console]::Out.WriteLine('NONE') }
}
`

var speech struct {
	sync.Mutex
	cmd     *exec.Cmd
	in      io.WriteCloser
	out     *bufio.Reader
	phrases string
}

// listen hears one command; "" if nothing was said.
func listen(phrases []string) (string, error) {
	speech.Lock()
	defer speech.Unlock()
	joined := strings.Join(phrases, "\n")
	if speech.cmd != nil && speech.phrases != joined { // new phone names: start afresh
		stopSpeech()
	}
	if speech.cmd == nil {
		if err := startSpeech(joined); err != nil {
			return "", err
		}
	}
	if _, err := io.WriteString(speech.in, "listen\n"); err != nil {
		stopSpeech()
		return "", errors.New("the speech helper stopped; try again")
	}
	line, err := readLine(15 * time.Second)
	if err != nil {
		stopSpeech()
		return "", err
	}
	if !strings.HasPrefix(line, "HEARD\t") {
		return "", nil
	}
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) < 3 {
		return "", nil
	}
	logf("voice: heard %q (confidence %s)", parts[2], parts[1])
	return parts[2], nil
}

func startSpeech(phrases string) error {
	dir := filepath.Join(os.TempDir(), "lanshare-speech")
	os.MkdirAll(dir, 0o700)
	script, list := filepath.Join(dir, "listen.ps1"), filepath.Join(dir, "phrases.txt")
	if err := os.WriteFile(script, []byte(speechScript), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(list, []byte(phrases), 0o600); err != nil {
		return err
	}
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script, list)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	in, _ := cmd.StdinPipe()
	outp, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return errors.New("couldn't start Windows speech recognition")
	}
	speech.cmd, speech.in, speech.out, speech.phrases = cmd, in, bufio.NewReader(outp), phrases
	line, err := readLine(30 * time.Second)
	switch {
	case err != nil:
		stopSpeech()
		return err
	case line == "ERR no-recognizer":
		stopSpeech()
		return errors.New("Windows speech recognition for English isn't installed (Settings → Time & language → Language → English → Speech).")
	case line == "ERR no-mic":
		stopSpeech()
		return errors.New("No microphone found.")
	case !strings.HasPrefix(line, "READY"):
		stopSpeech()
		return errors.New("Windows speech recognition didn't start.")
	}
	logf("voice: ready (%s)", strings.TrimPrefix(line, "READY "))
	return nil
}

func readLine(timeout time.Duration) (string, error) {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	r := speech.out
	go func() { s, err := r.ReadString('\n'); ch <- res{strings.TrimSpace(s), err} }()
	select {
	case x := <-ch:
		if x.err != nil && x.s == "" {
			return "", errors.New("the speech helper stopped")
		}
		return x.s, nil
	case <-time.After(timeout):
		return "", errors.New("listening took too long")
	}
}

func stopSpeech() {
	if speech.cmd != nil {
		speech.in.Close()
		speech.cmd.Process.Kill()
		speech.cmd.Wait()
	}
	speech.cmd, speech.in, speech.out = nil, nil, nil
}
