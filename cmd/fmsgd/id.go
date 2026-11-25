package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

var idHTTPClient = newIDHTTPClient()

func newIDHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 32
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
	}
}

type AddressDetail struct {
	Address             string   `json:"address"`
	DisplayName         string   `json:"displayName"`
	AcceptingNew        bool     `json:"acceptingNew"`
	LimitRecvSizeTotal  int64    `json:"limitRecvSizeTotal"`
	LimitRecvSizePerMsg int64    `json:"limitRecvSizePerMsg"`
	LimitRecvSizePer1d  int64    `json:"limitRecvSizePer1d"`
	LimitRecvCountPer1d int64    `json:"limitRecvCountPer1d"`
	LimitSendSizeTotal  int64    `json:"limitSendSizeTotal"`
	LimitSendSizePerMsg int64    `json:"limitSendSizePerMsg"`
	LimitSendSizePer1d  int64    `json:"limitSendSizePer1d"`
	LimitSendCountPer1d int64    `json:"limitSendCountPer1d"`
	RecvSizeTotal       int64    `json:"recvSizeTotal"`
	RecvSizePer1d       int64    `json:"recvSizePer1d"`
	RecvCountPer1d      int64    `json:"recvCountPer1d"`
	SendSizeTotal       int64    `json:"sendSizeTotal"`
	SendSizePer1d       int64    `json:"sendSizePer1d"`
	SendCountPer1d      int64    `json:"sendCountPer1d"`
	Tags                []string `json:"tags"`
}

// Returns pointer to an AddressDetail populated by querying fmsg Id standard at FMSG_ID_URL for
// address supplied. If the address is not found returns nil, nil.
func getAddressDetail(addr *FMsgAddress) (*AddressDetail, error) {
	uri := IDURI + "/fmsgid/" + url.PathEscape(addr.ToString())
	resp, err := idHTTPClient.Get(uri)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d", uri, resp.StatusCode)
	}

	var detail AddressDetail
	err = json.NewDecoder(resp.Body).Decode(&detail)
	if err != nil {
		return nil, err
	}

	return &detail, nil
}

func postMsgStatSend(addr *FMsgAddress, timestamp float64, size int64) error {
	return postMsgStat(addr, timestamp, size, true)
}

func postMsgStatRecv(addr *FMsgAddress, timestamp float64, size int64) error {
	return postMsgStat(addr, timestamp, size, false)
}

func postMsgStat(addr *FMsgAddress, timestamp float64, size int64, isSending bool) error {
	var part string
	if isSending {
		part = "send"
	} else {
		part = "recv"
	}
	uri := fmt.Sprintf("%s/fmsgid/%s", IDURI, part)

	payload := map[string]any{
		"address": addr.ToString(),
		"ts":      timestamp,
		"size":    size}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	resp, err := idHTTPClient.Post(uri, "application/json", bytes.NewReader(jsonPayload))
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s returned %d", uri, resp.StatusCode)
	}

	return nil
}
