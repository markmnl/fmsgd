package main

import (
	"database/sql"

	"github.com/levenlabs/golib/timeutil"
	"github.com/lib/pq"
)

func testDb() error {
	db, err := sql.Open("postgres", "")
	if err != nil {
		return err
	}
	defer db.Close()
	err = db.Ping()
	if err != nil {
		return err
	}
	// TODO check at least tables exist?
	return nil
}

func storeMsgDetail(msg *FMsgHeader) error {

	db, err := sql.Open("postgres", "")
	if err != nil {
		return err
	}
	defer db.Close()

	stmt, err := db.Prepare(`insert into msg (version	
	, flags
	, time_sent
	, time_recv
	, from_addr
	, to_addrs
	, topic
	, type
	, sha256
	, psha256
	, size
	, filepath) 
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
returning id`)
	if err != nil {
		return err
	}

	count := len(msg.To)
	to_addrs := make([]string, count)
	for i := 0; i < count; i++ {
		to_addrs[i] = msg.To[i].ToString()
	}

	msgHash, err := msg.GetMessageHash()
	if err != nil {
		return err
	}

	_, err = stmt.Exec(msg.Version,
		msg.Flags,
		msg.Timestamp,
		timeutil.TimestampNow().Float64(),
		msg.From.ToString(),
		pq.Array(to_addrs),
		msg.Topic,
		msg.Type,
		msgHash,
		msg.Pid,
		int(msg.Size),
		msg.Filepath)
	if err != nil {
		return err
	}
	// TODO set pid if psha256
	// TODO attachments

	return nil

}
