package main

import (
	"database/sql"

	"github.com/levenlabs/golib/timeutil"
	"github.com/lib/pq"
)

var dbName = "fmsg"
var sqlTables = `create table if not exists msg (
    id            	bigserial       	primary key,
	version			int					not null,
    pid           	bigint          	references msg (id),
	flags		  	int					not null,
    time_sent     	double precision 	not null,
	time_recv     	double precision 	not null,
    from_addr     	varchar(255)    	not null,
    to_addrs       	varchar(255)[]  	not null,
    topic         	varchar(255)    	not null, 
    type          	varchar(255)    	not null,
    sha256        	bytea           	unique not null,
    psha256       	bytea,
	size			int					not null, -- spec allows uint32 but we don't enforced by FMSG_MAX_MSG_SIZE
    filepath      	text            	not null
);
 
create table if not exists msg_attachment (
    msg_id        	bigint          references msg (id),
    filename      	varchar(255)    not null,
    filesize      	int             not null, 
    filepath      	text			not null,
    primary key (msg_id, filename)
);`

func initDb() error {
	db, err := sql.Open("postgres", "dbname="+dbName)
	if err != nil {
		return err
	}
	defer db.Close()
	err = db.Ping()
	if err != nil {
		return err
	}
	_, err = db.Exec(sqlTables)
	if err != nil {
		return err
	}
	return nil
}

func storeMsgDetail(msg *FMsgHeader) error {
	
	db, err := sql.Open("postgres", "dbname="+dbName)
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
	, filepath) 
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
returning id`)
	if err != nil {
		return err
	}

	count:= len(msg.To)
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
		msg.Filepath)
    if err != nil {
        return err
    }
	// TODO set pid if psha256
	// TODO attachments

	return nil

}