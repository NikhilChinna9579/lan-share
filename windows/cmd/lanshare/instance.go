package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// instance is how a second copy of lanshare.exe finds the one already running: the
// running copy writes its local address and a secret token to instance.json (readable
// only by this Windows user), and the newcomer passes its request on and exits.
type instance struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

func instanceFile(dataDir string) string { return filepath.Join(dataDir, "instance.json") }

func writeInstance(dataDir string, in instance) error {
	data, _ := json.Marshal(in)
	return os.WriteFile(instanceFile(dataDir), data, 0o600)
}

func findRunning(dataDir string) (*instance, bool) {
	data, err := os.ReadFile(instanceFile(dataDir))
	if err != nil {
		return nil, false
	}
	var in instance
	if json.Unmarshal(data, &in) != nil || in.Port == 0 {
		return nil, false
	}
	if in.call("/api/ping", nil) != nil {
		return nil, false // stale file from a copy that has exited
	}
	return &in, true
}

func (in *instance) call(path string, body any) error {
	_, err := in.request(path, body, 3*time.Second)
	return err
}

func (in *instance) request(path string, body any, timeout time.Duration) ([]byte, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d%s", in.Port, path), bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Token", in.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return out, fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(out))
	}
	return out, nil
}

// sendNow (testing) asks the running copy to send files and waits for the result.
func (in *instance) sendNow(files []string, folder string) error {
	out, err := in.request("/api/send-now", map[string]any{"files": files, "folder": folder}, 2*time.Minute)
	fmt.Println(string(bytes.TrimSpace(out)))
	return err
}
