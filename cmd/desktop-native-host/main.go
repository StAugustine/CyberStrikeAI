package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"cyberstrike-ai/internal/desktopplugin"
)

const maximumNativeMessageBytes = 16 * 1024

type nativeRequest struct {
	Operation string `json:"operation"`
}

type nativeResponse struct {
	OK        bool                     `json:"ok"`
	Discovery *desktopplugin.Discovery `json:"discovery,omitempty"`
	Error     string                   `json:"error,omitempty"`
}

func main() {
	path, err := desktopplugin.DefaultDiscoveryPath()
	if err != nil {
		_ = writeNativeResponse(os.Stdout, nativeResponse{Error: "desktop integration is unavailable"})
		return
	}
	if err := runNativeHost(os.Stdin, os.Stdout, path, time.Now()); err != nil {
		_ = writeNativeResponse(os.Stdout, nativeResponse{Error: "desktop integration request is invalid"})
	}
}

func runNativeHost(input io.Reader, output io.Writer, discoveryPath string, now time.Time) error {
	payload, err := readNativeMessage(input)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var request nativeRequest
	if err := decoder.Decode(&request); err != nil || request.Operation != "discover" {
		return errors.New("unsupported native messaging operation")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("native messaging request contains trailing data")
	}
	discovery, err := desktopplugin.LoadDiscovery(discoveryPath, now)
	if err != nil {
		return writeNativeResponse(output, nativeResponse{Error: "desktop integration is unavailable"})
	}
	return writeNativeResponse(output, nativeResponse{OK: true, Discovery: &discovery})
}

func readNativeMessage(input io.Reader) ([]byte, error) {
	var size uint32
	if err := binary.Read(input, binary.LittleEndian, &size); err != nil {
		return nil, err
	}
	if size == 0 || size > maximumNativeMessageBytes {
		return nil, errors.New("native messaging request size is invalid")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(input, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func writeNativeResponse(output io.Writer, response nativeResponse) error {
	payload, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if len(payload) > maximumNativeMessageBytes {
		return errors.New("native messaging response size is invalid")
	}
	if err := binary.Write(output, binary.LittleEndian, uint32(len(payload))); err != nil {
		return err
	}
	_, err = output.Write(payload)
	return err
}
