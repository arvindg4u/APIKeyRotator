package services

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"testing"
)

func gzipTestBody(t *testing.T, body []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(body); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return compressed.Bytes()
}

func TestBufferedResponseDecompressesGzip(t *testing.T) {
	want := []byte(`{"error":{"message":"quota exhausted"}}`)
	buffered := &BufferedResponse{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Content-Type":     []string{"application/json"},
			"Content-Encoding": []string{"gzip"},
			"Content-Length":   []string{"123"},
		},
		Body: gzipTestBody(t, want),
	}

	resp := buffered.HTTPResponse()
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" {
		t.Fatalf("Content-Encoding = %q after decompression, want empty", encoding)
	}
	if length := resp.Header.Get("Content-Length"); length != "" {
		t.Fatalf("Content-Length = %q after buffering, want empty", length)
	}
}

func TestBufferedResponsePreservesGzipWhenDecodeFails(t *testing.T) {
	body := []byte("this is not gzip")
	buffered := &BufferedResponse{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Content-Encoding": []string{"gzip"},
			"Content-Length":   []string{"16"},
		},
		Body: body,
	}

	resp := buffered.HTTPResponse()
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body = %q, want the original bytes", got)
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip when decompression fails", encoding)
	}
}
