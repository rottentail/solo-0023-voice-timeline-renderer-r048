package xarch

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// This file runs the real voice-timeline service binary for one architecture
// against the materialized suite and records the downloaded outputs and
// receipts as a per-architecture bundle. It only speaks the public HTTP API;
// it never links the production audio packages.

// RunnerConfig tells RunArchitecture which binary to start and how.
type RunnerConfig struct {
	Arch       string // label recorded in the bundle, e.g. "amd64"
	BinaryPath string // path to the voiceservice executable for that arch
	QEMUBin    string // optional qemu user emulator (e.g. qemu-x86_64-static); empty = native
	WorkDir    string // scratch directory for the service data root
	BundleDir  string // output directory for this architecture's bundle
	FixtureDir string // materialized, digest-pinned WAV fixtures
	StartWait  time.Duration
	HTTPClient *http.Client
}

type sourceWriteRequest struct {
	Name      string `json:"name"`
	WAVBase64 string `json:"wav_base64"`
}

type timelineWriteRequest struct {
	Name           string     `json:"name"`
	SampleRate     int        `json:"sample_rate"`
	OutputChannels int        `json:"output_channels"`
	TargetPeakDBFS float64    `json:"target_peak_dbfs"`
	Spans          []SpanSpec `json:"spans"`
}

type renderWriteRequest struct {
	TimelineID string `json:"timeline_id"`
}

type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Field   string `json:"field"`
	} `json:"error"`
}

// RunArchitecture starts the service, replays the whole suite through the real
// HTTP endpoints, and writes a bundle directory containing each rendered WAV
// and receipt. Fixture digests are verified before anything is registered so a
// corrupted input set can never produce a misleading compatibility result.
func RunArchitecture(ctx context.Context, suite Suite, config RunnerConfig) (Bundle, error) {
	if err := verifyFixtureDigests(suite, config.FixtureDir); err != nil {
		return Bundle{}, fmt.Errorf("input facts failed digest verification: %w", err)
	}

	port, err := freePort()
	if err != nil {
		return Bundle{}, err
	}
	dataDir := filepath.Join(config.WorkDir, "data-"+config.Arch)
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return Bundle{}, err
	}
	if err := os.MkdirAll(config.BundleDir, 0o750); err != nil {
		return Bundle{}, err
	}

	cmd, baseURL, err := startService(ctx, config, dataDir, port)
	if err != nil {
		return Bundle{}, err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	wait := config.StartWait
	if wait <= 0 {
		wait = 20 * time.Second
	}
	if err := waitForHealth(ctx, baseURL, wait); err != nil {
		return Bundle{}, fmt.Errorf("service for arch %s never became healthy: %w", config.Arch, err)
	}

	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	runner := &httpRunner{
		ctx:     ctx,
		client:  client,
		baseURL: baseURL,
	}

	for _, source := range suite.Sources {
		wavPath := filepath.Join(config.FixtureDir, source.FileName)
		wav, err := os.ReadFile(wavPath)
		if err != nil {
			return Bundle{}, fmt.Errorf("read fixture %s: %w", source.ID, err)
		}
		if status, body, err := runner.put("/v1/sources/"+source.ID, sourceWriteRequest{
			Name:      source.ID,
			WAVBase64: base64.StdEncoding.EncodeToString(wav),
		}); err != nil {
			return Bundle{}, err
		} else if status != http.StatusCreated && status != http.StatusOK {
			return Bundle{}, fmt.Errorf("register source %s on %s failed: status %d %s", source.ID, config.Arch, status, errorText(body))
		}
	}

	bundle := Bundle{
		Arch:     config.Arch,
		Outputs:  make(map[string][]byte),
		Receipts: make(map[string]RenderReceiptDoc),
	}
	for index, timeline := range suite.Timelines {
		if status, body, err := runner.put(fmt.Sprintf("/v1/timelines/%s", timeline.ID), timelineWriteRequest{
			Name:           timeline.Name,
			SampleRate:     timeline.SampleRate,
			OutputChannels: timeline.OutputChannels,
			TargetPeakDBFS: timeline.TargetPeakDBFS,
			Spans:          timeline.Spans,
		}); err != nil {
			return Bundle{}, err
		} else if status != http.StatusCreated && status != http.StatusOK {
			return Bundle{}, fmt.Errorf("create timeline %s on %s failed: status %d %s", timeline.ID, config.Arch, status, errorText(body))
		}

		renderID := fmt.Sprintf("render_%s_%d", config.Arch, index)
		if status, body, err := runner.put("/v1/renders/"+renderID, renderWriteRequest{TimelineID: timeline.ID}); err != nil {
			return Bundle{}, err
		} else if status != http.StatusCreated && status != http.StatusOK {
			return Bundle{}, fmt.Errorf("render %s on %s failed: status %d %s", renderID, config.Arch, status, errorText(body))
		}

		output, status, body, err := runner.get("/v1/renders/" + renderID + "/output")
		if err != nil {
			return Bundle{}, err
		}
		if status != http.StatusOK {
			return Bundle{}, fmt.Errorf("download output %s on %s failed: status %d %s", renderID, config.Arch, status, errorText(body))
		}
		receiptBytes, status, body, err := runner.get("/v1/renders/" + renderID + "/receipt")
		if err != nil {
			return Bundle{}, err
		}
		if status != http.StatusOK {
			return Bundle{}, fmt.Errorf("read receipt %s on %s failed: status %d %s", renderID, config.Arch, status, errorText(body))
		}
		var receipt RenderReceiptDoc
		if err := json.Unmarshal(receiptBytes, &receipt); err != nil {
			return Bundle{}, fmt.Errorf("parse receipt for %s on %s: %w", renderID, config.Arch, err)
		}

		outputPath := filepath.Join(config.BundleDir, timeline.ID+".wav")
		if err := os.WriteFile(outputPath, output, 0o640); err != nil {
			return Bundle{}, err
		}
		receiptPath := filepath.Join(config.BundleDir, timeline.ID+".receipt.json")
		if err := os.WriteFile(receiptPath, receiptBytes, 0o640); err != nil {
			return Bundle{}, err
		}
		bundle.Outputs[timeline.ID] = output
		bundle.Receipts[timeline.ID] = receipt
	}

	manifest := map[string]string{"arch": config.Arch, "suite_id": suite.SuiteID}
	manifestBytes, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(config.BundleDir, "bundle.json"), manifestBytes, 0o640); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

func verifyFixtureDigests(suite Suite, fixtureDir string) error {
	for _, source := range suite.Sources {
		data, err := os.ReadFile(filepath.Join(fixtureDir, source.FileName))
		if err != nil {
			return fmt.Errorf("fixture %s missing: %w", source.ID, err)
		}
		actual := SHA256Hex(data)
		if actual != source.Digest {
			return fmt.Errorf("fixture %s digest %s does not match pinned suite digest %s", source.ID, actual, source.Digest)
		}
	}
	return nil
}

type httpRunner struct {
	ctx     context.Context
	client  *http.Client
	baseURL string
}

func (r *httpRunner) put(path string, payload any) (int, []byte, error) {
	return r.do(http.MethodPut, path, payload)
}

func (r *httpRunner) get(path string) ([]byte, int, []byte, error) {
	status, body, err := r.do(http.MethodGet, path, nil)
	return body, status, body, err
}

func (r *httpRunner) do(method, path string, payload any) (int, []byte, error) {
	var bodyReader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		bodyReader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(r.ctx, method, r.baseURL+path, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := r.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, nil, err
	}
	return response.StatusCode, body, nil
}

func errorText(body []byte) string {
	var envelope errorEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Code != "" {
		return envelope.Error.Code
	}
	return string(body)
}

func startService(ctx context.Context, config RunnerConfig, dataDir string, port int) (*exec.Cmd, string, error) {
	address := fmt.Sprintf("127.0.0.1:%d", port)
	var args []string
	if config.QEMUBin != "" {
		args = append(args, config.BinaryPath)
	}
	args = append(args, "-addr", address, "-data", dataDir)

	var cmd *exec.Cmd
	if config.QEMUBin != "" {
		cmd = exec.CommandContext(ctx, config.QEMUBin, args...)
	} else {
		cmd = exec.CommandContext(ctx, config.BinaryPath, args...)
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("start %s service: %w", config.Arch, err)
	}
	return cmd, "http://" + address, nil
}

func waitForHealth(ctx context.Context, baseURL string, limit time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("health check did not succeed within %s", limit)
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
