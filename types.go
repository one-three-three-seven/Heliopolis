package main

import (
	"encoding/json"
	"time"
)

type EngineRequest struct {
	Id      int64           `json:"id"`
	JsonRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type EngineResponse struct {
	Id      int64           `json:"id"`
	JsonRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
}

type ForkchoiceStateV1 struct {
	Status          string `json:"status"`
	LatestValidHash string `json:"latestValidHash"`
	ValidationError string `json:"validationError"`
}

type PayloadStatusV1 struct {
	PayloadStatus struct {
		Status          string `json:"status"`
		LatestValidHash string `json:"latestValidHash"`
		ValidationError string `json:"validationError"`
	} `json:"payloadStatus"`
	PayloadId string `json:"payloadId"`
}

type Engine struct {
	Url         string
	BlockHeight int64
}

type BlockNumberResponse struct {
	Id      int    `json:"id"`
	Jsonrpc string `json:"jsonrpc"`
	Result  string `json:"result"`
}

type Result struct {
	Engine   string
	Status   string
	Duration time.Duration
	Response []byte
}
