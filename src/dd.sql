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
create index on msg ((lower(from_addr)));

create table if not exists msg_to (
	msg_id			bigint				not null,
	addr			varchar(255)		not null,
	primary key (msg_id, addr)
);
create index on msg_to ((lower(addr)));

create or replace function add_msg_to_addrs() returns trigger as $$
	begin
		for i in 1..array_upper(NEW.to_addrs, 1) loop
			insert into msg_to(msg_id, addr) values (NEW.id, NEW.to_addrs[i]);
		end loop;
        return null;
	end;
$$ language plpgsql;

create trigger on_msg_insert after insert on msg
	for each row
	execute function add_msg_to_addrs();
 
create table if not exists msg_attachment (
    msg_id        	bigint          references msg (id),
    filename      	varchar(255)    not null,
    filesize      	int             not null, 
    filepath      	text			not null,
    primary key (msg_id, filename)
);