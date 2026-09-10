package main

import "github.com/markmnl/fmsgd/pkg/fmsg"

const deflateSampleSize = 8192

var shouldCompress = fmsg.ShouldCompress
var tryCompress = fmsg.TryCompress
