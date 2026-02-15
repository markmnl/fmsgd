package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	env "github.com/caitlinelfring/go-env-default"
	"github.com/lib/pq"
)

var RetryInterval float64 = 20
var PollInterval = 10
var MaxConcurrentSend = 32

func loadSenderEnvConfig() {
	RetryInterval = env.GetFloatDefault("FMSG_RETRY_INTERVAL", 20)
	PollInterval = env.GetIntDefault("FMSG_POLL_INTERVAL", 10)
	MaxConcurrentSend = env.GetIntDefault("FMSG_MAX_CONCURRENT_SEND", 32)
}

// deliverToHost sends a message to a remote fmsg host for the given domain and
// recipients, following the fmsg wire protocol. It updates the database with
// the result for each recipient.
func deliverToHost(pm *PendingMessage, domain string, recipients []PendingRecipient) {
	addrs := make([]string, len(recipients))
	for i, r := range recipients {
		addrs[i] = r.Addr
	}

	// resolve _fmsg.<domain> for the target host
	targetIPs, err := lookupAuthorisedIPs(domain)
	if err != nil {
		log.Printf("ERROR: sender: DNS lookup for _fmsg.%s failed: %s", domain, err)
		for _, r := range recipients {
			if err := updateLastAttempt(r.MsgID, r.Addr, nil); err != nil {
				log.Printf("ERROR: sender: updating last attempt for %s: %s", r.Addr, err)
			}
		}
		return
	}

	// build header
	h, err := pm.ToFMsgHeader(addrs)
	if err != nil {
		log.Printf("ERROR: sender: building header for msg %d: %s", pm.MsgID, err)
		return
	}

	// register in outgoing map so we can respond to challenge
	hash := h.GetHeaderHash()
	hashArr := *(*[32]byte)(hash)
	outgoing[hashArr] = h
	defer delete(outgoing, hashArr)

	// try connecting to each resolved IP until one works
	var conn net.Conn
	for _, ip := range targetIPs {
		addr := net.JoinHostPort(ip.String(), fmt.Sprintf("%d", Port))
		conn, err = net.DialTimeout("tcp", addr, 15*time.Second)
		if err == nil {
			break
		}
		log.Printf("WARN: sender: failed to connect to %s: %s", addr, err)
	}
	if conn == nil {
		log.Printf("ERROR: sender: could not connect to any IP for _fmsg.%s", domain)
		for _, r := range recipients {
			if err := updateLastAttempt(r.MsgID, r.Addr, nil); err != nil {
				log.Printf("ERROR: sender: updating last attempt for %s: %s", r.Addr, err)
			}
		}
		return
	}
	defer conn.Close()

	// encode and send the header

	// version
	if err := binary.Write(conn, binary.LittleEndian, h.Version); err != nil {
		log.Printf("ERROR: sender: writing version: %s", err)
		return
	}

	// flags
	if err := binary.Write(conn, binary.LittleEndian, h.Flags); err != nil {
		log.Printf("ERROR: sender: writing flags: %s", err)
		return
	}

	// pid
	if h.Flags&FlagHasPid == 1 {
		if _, err := conn.Write(h.Pid); err != nil {
			log.Printf("ERROR: sender: writing pid: %s", err)
			return
		}
	}

	// from
	fromStr := h.From.ToString()
	if err := binary.Write(conn, binary.LittleEndian, uint8(len(fromStr))); err != nil {
		log.Printf("ERROR: sender: writing from length: %s", err)
		return
	}
	if _, err := conn.Write([]byte(fromStr)); err != nil {
		log.Printf("ERROR: sender: writing from: %s", err)
		return
	}

	// to count and addresses
	if err := binary.Write(conn, binary.LittleEndian, uint8(len(h.To))); err != nil {
		log.Printf("ERROR: sender: writing to count: %s", err)
		return
	}
	for _, addr := range h.To {
		s := addr.ToString()
		if err := binary.Write(conn, binary.LittleEndian, uint8(len(s))); err != nil {
			log.Printf("ERROR: sender: writing to addr length: %s", err)
			return
		}
		if _, err := conn.Write([]byte(s)); err != nil {
			log.Printf("ERROR: sender: writing to addr: %s", err)
			return
		}
	}

	// timestamp
	if err := binary.Write(conn, binary.LittleEndian, h.Timestamp); err != nil {
		log.Printf("ERROR: sender: writing timestamp: %s", err)
		return
	}

	// topic
	if err := binary.Write(conn, binary.LittleEndian, uint8(len(h.Topic))); err != nil {
		log.Printf("ERROR: sender: writing topic length: %s", err)
		return
	}
	if _, err := conn.Write([]byte(h.Topic)); err != nil {
		log.Printf("ERROR: sender: writing topic: %s", err)
		return
	}

	// type
	if err := binary.Write(conn, binary.LittleEndian, uint8(len(h.Type))); err != nil {
		log.Printf("ERROR: sender: writing type length: %s", err)
		return
	}
	if _, err := conn.Write([]byte(h.Type)); err != nil {
		log.Printf("ERROR: sender: writing type: %s", err)
		return
	}

	// size
	if err := binary.Write(conn, binary.LittleEndian, h.Size); err != nil {
		log.Printf("ERROR: sender: writing size: %s", err)
		return
	}

	// TODO attachment_count and attachment headers
	if err := binary.Write(conn, binary.LittleEndian, uint8(0)); err != nil {
		log.Printf("ERROR: sender: writing attachment count: %s", err)
		return
	}

	// send message data
	fd, err := os.Open(h.Filepath)
	if err != nil {
		log.Printf("ERROR: sender: opening message file %s: %s", h.Filepath, err)
		return
	}
	defer fd.Close()

	d := calcNetIODuration(int(h.Size), MinUploadRate)
	conn.SetWriteDeadline(time.Now().Add(d))
	n, err := io.CopyN(conn, fd, int64(h.Size))
	if err != nil {
		log.Printf("ERROR: sender: sending message data (%d/%d bytes): %s", n, h.Size, err)
		return
	}

	// TODO send attachment bodies

	// read accept/reject response
	// A code < 100 means rejection for ALL recipients (single byte).
	// Otherwise one code per recipient in order.
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	firstByte := make([]byte, 1)
	if _, err := io.ReadFull(conn, firstByte); err != nil {
		log.Printf("ERROR: sender: reading response: %s", err)
		for _, r := range recipients {
			if err := updateLastAttempt(r.MsgID, r.Addr, nil); err != nil {
				log.Printf("ERROR: sender: updating last attempt for %s: %s", r.Addr, err)
			}
		}
		return
	}

	code := firstByte[0]
	if code < 100 {
		// global rejection for all recipients
		log.Printf("WARN: sender: msg %d globally rejected by %s with code %d", pm.MsgID, domain, code)
		for _, r := range recipients {
			if err := updateLastAttempt(r.MsgID, r.Addr, &code); err != nil {
				log.Printf("ERROR: sender: updating last attempt for %s: %s", r.Addr, err)
			}
		}
		return
	}

	// first byte is per-recipient code for recipients[0]
	codes := make([]byte, len(recipients))
	codes[0] = code
	if len(recipients) > 1 {
		remaining := make([]byte, len(recipients)-1)
		if _, err := io.ReadFull(conn, remaining); err != nil {
			log.Printf("ERROR: sender: reading remaining response codes: %s", err)
			for _, r := range recipients {
				if err := updateLastAttempt(r.MsgID, r.Addr, nil); err != nil {
					log.Printf("ERROR: sender: updating last attempt for %s: %s", r.Addr, err)
				}
			}
			return
		}
		copy(codes[1:], remaining)
	}

	// process per-recipient codes
	for i, r := range recipients {
		c := codes[i]
		if c == 200 {
			log.Printf("INFO: sender: delivered msg %d to %s", pm.MsgID, r.Addr)
			if err := updateDelivered(r.MsgID, r.Addr); err != nil {
				log.Printf("ERROR: sender: updating delivered for %s: %s", r.Addr, err)
			}
		} else {
			log.Printf("WARN: sender: msg %d to %s rejected with code %d", pm.MsgID, r.Addr, c)
			if err := updateLastAttempt(r.MsgID, r.Addr, &c); err != nil {
				log.Printf("ERROR: sender: updating last attempt for %s: %s", r.Addr, err)
			}
		}
	}
}

// processPendingMessages queries for messages that need delivery and dispatches
// a goroutine per (message, domain) pair, bounded by the semaphore.
func processPendingMessages(sem chan struct{}, wg *sync.WaitGroup) {
	pending, err := selectPendingMessages(RetryInterval)
	if err != nil {
		log.Printf("ERROR: sender: querying pending messages: %s", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	log.Printf("INFO: sender: found %d pending message(s)", len(pending))

	for i := range pending {
		pm := &pending[i]
		groups := groupRecipientsByDomain(pm)
		for domain, recipients := range groups {
			sem <- struct{}{} // acquire
			wg.Add(1)
			go func(pm *PendingMessage, domain string, recipients []PendingRecipient) {
				defer wg.Done()
				defer func() { <-sem }() // release
				deliverToHost(pm, domain, recipients)
			}(pm, domain, recipients)
		}
	}
}

// startSender runs the sender loop: polls the database periodically and also
// listens for PostgreSQL notifications for immediate pickup of new messages.
func startSender() {
	loadSenderEnvConfig()
	log.Printf("INFO: sender: started (poll=%ds, retry=%.0fs, max_concurrent=%d)",
		PollInterval, RetryInterval, MaxConcurrentSend)

	sem := make(chan struct{}, MaxConcurrentSend)
	var wg sync.WaitGroup

	// set up PostgreSQL LISTEN for immediate notification
	notifyCh := make(chan struct{}, 1) // buffered so we don't block the listener
	go func() {
		listener := pq.NewListener("", 10*time.Second, time.Minute, func(ev pq.ListenerEventType, err error) {
			if err != nil {
				log.Printf("ERROR: sender: pg listener: %s", err)
			}
		})
		if err := listener.Listen("new_msg_to"); err != nil {
			log.Printf("ERROR: sender: could not LISTEN on new_msg_to: %s", err)
			return
		}
		defer listener.Close()
		log.Println("INFO: sender: listening for new_msg_to notifications")
		for {
			select {
			case n := <-listener.Notify:
				if n != nil {
					log.Printf("INFO: sender: notification received: %s", n.Extra)
					select {
					case notifyCh <- struct{}{}:
					default:
					}
				}
			case <-time.After(90 * time.Second):
				// ping to keep connection alive
				if err := listener.Ping(); err != nil {
					log.Printf("ERROR: sender: pg listener ping: %s", err)
				}
			}
		}
	}()

	// initial poll on startup
	processPendingMessages(sem, &wg)

	ticker := time.NewTicker(time.Duration(PollInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			processPendingMessages(sem, &wg)
		case <-notifyCh:
			// small delay to batch rapid inserts
			time.Sleep(500 * time.Millisecond)
			processPendingMessages(sem, &wg)
		}
	}
}
