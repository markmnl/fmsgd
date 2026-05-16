package main

import "github.com/markmnl/fmsgd/pkg/fmsg"

// Type aliases so all internal code continues to use unqualified names.
type FMsgAddress = fmsg.Address
type FMsgAttachmentHeader = fmsg.AttachmentHeader
type FMsgHeader = fmsg.Header

// Flag constants forwarded from the fmsg package.
const (
	FlagHasPid     = fmsg.FlagHasPid
	FlagHasAddTo   = fmsg.FlagHasAddTo
	FlagCommonType = fmsg.FlagCommonType
	FlagImportant  = fmsg.FlagImportant
	FlagNoReply    = fmsg.FlagNoReply
	FlagDeflate    = fmsg.FlagDeflate
)
