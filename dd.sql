/****************************************************************
 *
 * PostgreSQL database objects data definition for fmsgd
 *
 ****************************************************************/

-- database with encoding UTF8 should already be created and connected

create table if not exists msg (
    id            	bigserial       	primary key,
	version			int					not null,
    pid           	bigint          	references msg (id),
	flags		  	int					not null,
    time_sent     	double precision 	not null, -- time sending host recieved message for sending, message timestamp field
	time_recv     	double precision 	not null, -- time recieving host downloaded the message
    from_addr     	varchar(255)    	not null,
    topic         	varchar(255)    	not null, 
    type          	varchar(255)    	not null,
    sha256        	bytea           	unique not null,
    psha256       	bytea,
	size			int					not null, -- spec allows uint32 but we don't enforced by FMSG_MAX_MSG_SIZE
    filepath      	text            	not null
);
create index on msg ((lower(from_addr)));

create table if not exists msg_to (
	msg_id			bigint				references msg (id),
	addr			varchar(255)		not null,
    status          smallint,
	primary key (msg_id, addr)
);
create index on msg_to ((lower(addr)));

create table if not exists msg_attachment (
    msg_id        	bigint          references msg (id),
    filename      	varchar(255)    not null,
    filesize      	int             not null, 
    filepath      	text			not null,
    primary key (msg_id, filename)
);
