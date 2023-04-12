package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
)

type AddressDetail struct {
    Name              	string 	`json:"name"`
    Address           	string 	`json:"address"`
    DisplayName       	string 	`json:"displayName"`
    AcceptingNew      	bool   	`json:"acceptingNew"`
    LimitRecvSizeTotal 	int    	`json:"limitRecvSizeTotal"`
    LimitRecvSizePerMsg int    	`json:"limitRecvSizePerMsg"`
    LimitRecvSizePer1d 	int    	`json:"limitRecvSizePer1d"`
    LimitRecvCountPer1d int    	`json:"limitRecvCountPer1d"`
    LimitSendSizeTotal 	int    	`json:"limitSendSizeTotal"`
    LimitSendSizePerMsg int    	`json:"limitSendSizePerMsg"`
    LimitSendSizePer1d 	int    	`json:"limitSendSizePer1d"`
    LimitSendCountPer1d int   	`json:"limitSendCountPer1d"`
    RecvSizeTotal      	int     `json:"recvSizeTotal"`
    RecvSizePer1d      	int     `json:"recvSizePer1d"`
    RecvCountPer1d     	int     `json:"recvCountPer1d"`
    SendSizeTotal      	int     `json:"sendSizeTotal"`
    SendSizePer1d      	int     `json:"sendSizePer1d"`
    SendCountPer1d     	int     `json:"sendCountPer1d"`
    Tags              	[]string  `json:"tags"`
}

func getAddressDetail(addr *FMsgAddress) (*AddressDetail, error) {
	uri := path.Join(IDURI, "user", addr.ToString())
    resp, err := http.Get(uri)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	var detail AddressDetail
    err = json.NewDecoder(resp.Body).Decode(&detail)
    if err != nil {
        return nil, err
    }

    return &detail, nil
}

func postMsgStatSend(addr *FMsgAddress, timestamp float64, size int) error {
	return postMsgStat(addr, timestamp, size, true)
}

func postMsgStatRecv(addr *FMsgAddress, timestamp float64, size int) error {
	return postMsgStat(addr, timestamp, size, false)
}

func postMsgStat(addr *FMsgAddress, timestamp float64, size int, isSending bool) error {
	var part string
	if isSending {
		part = "send"
	} else {
		part = "recv"
	}
	uri := path.Join(IDURI, "user", part, addr.ToString())

	payload := []byte(fmt.Sprintf(`{"timestamp": %f, "size": %d}`, timestamp, size))
    jsonPayload := json.RawMessage(payload)

    resp, err := http.Post(uri, "application/json", bytes.NewBuffer(jsonPayload))
    if err != nil {
        return err
    }
    defer resp.Body.Close()

	return nil
}