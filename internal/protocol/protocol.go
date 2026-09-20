// Package protocol defines the durable wire contract between collectors and hubs.
package protocol

import "time"

const Version = 2
const MaxChunkBytes = 1024 * 1024

type Source struct {
	MachineID          string    `json:"machineId"`
	SourceID           string    `json:"sourceId"`
	Generation         string    `json:"generation"`
	GenerationSequence int64     `json:"generationSequence"`
	Provider           string    `json:"provider"`
	NativeID           string    `json:"nativeId"`
	Path               string    `json:"path"`
	Size               int64     `json:"size"`
	ModifiedAt         time.Time `json:"modifiedAt"`
}

type Chunk struct {
	Source Source `json:"source"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	SHA256 string `json:"sha256"`
}

type Receipt struct {
	ReceiptID     string `json:"receiptId"`
	SourceID      string `json:"sourceId"`
	Generation    string `json:"generation"`
	DurableOffset int64  `json:"durableOffset"`
	IndexedOffset int64  `json:"indexedOffset"`
	RecoveryEpoch string `json:"recoveryEpoch"`
}

type Heartbeat struct {
	Network         *NetworkReport `json:"network,omitempty"`
	MachineID       string         `json:"machineId"`
	Name            string         `json:"name"`
	Version         string         `json:"version"`
	BootID          string         `json:"bootId"`
	UptimeSeconds   int64          `json:"uptimeSeconds"`
	CapturedBytes   int64          `json:"capturedBytes"`
	UploadedBytes   int64          `json:"uploadedBytes"`
	BacklogBytes    int64          `json:"backlogBytes"`
	HistoryGaps     *int64         `json:"historyGaps,omitempty"`
	LastUploadAt    *time.Time     `json:"lastUploadAt,omitempty"`
	State           string         `json:"state"`
	CollectionState string         `json:"collectionState,omitempty"`
	ConnectionState string         `json:"connectionState,omitempty"`
	CollectionError string         `json:"collectionError,omitempty"`
	ConnectionError string         `json:"connectionError,omitempty"`
	UploadRetryAt   *time.Time     `json:"uploadRetryAt,omitempty"`
	Error           string         `json:"error,omitempty"`
}

type APIError struct {
	Code      string `json:"code"`
	Error     string `json:"error"`
	Retryable bool   `json:"retryable"`
}
