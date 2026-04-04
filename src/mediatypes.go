package main

// Common Media Types mapping defined in the fmsg SPECIFICATION.
// When the common-type flag is set, the type field on the wire is a single
// uint8 index into this table instead of a length-prefixed ASCII string.

// numberToMediaType maps a common type uint8 index to its full Media Type string
// exactly as listed in the fmsg specification Common Media Types table.
var numberToMediaType = map[uint8]string{
	1:  "application/epub+zip",
	2:  "application/gzip",
	3:  "application/json",
	4:  "application/msword",
	5:  "application/octet-stream",
	6:  "application/pdf",
	7:  "application/rtf",
	8:  "application/vnd.amazon.ebook",
	9:  "application/vnd.ms-excel",
	10: "application/vnd.ms-powerpoint",
	11: "application/vnd.oasis.opendocument.presentation",
	12: "application/vnd.oasis.opendocument.spreadsheet",
	13: "application/vnd.oasis.opendocument.text",
	14: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	15: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	16: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	17: "application/x-tar",
	18: "application/xhtml+xml",
	19: "application/xml",
	20: "application/zip",
	21: "audio/aac",
	22: "audio/midi",
	23: "audio/mpeg",
	24: "audio/ogg",
	25: "audio/opus",
	26: "audio/vnd.wave",
	27: "audio/webm",
	28: "font/otf",
	29: "font/ttf",
	30: "font/woff",
	31: "font/woff2",
	32: "image/apng",
	33: "image/avif",
	34: "image/bmp",
	35: "image/gif",
	36: "image/heic",
	37: "image/jpeg",
	38: "image/png",
	39: "image/svg+xml",
	40: "image/tiff",
	41: "image/webp",
	42: "model/3mf",
	43: "model/gltf-binary",
	44: "model/obj",
	45: "model/step",
	46: "model/stl",
	47: "model/vnd.usdz+zip",
	48: "text/calendar",
	49: "text/css",
	50: "text/csv",
	51: "text/html",
	52: "text/javascript",
	53: "text/markdown",
	54: "text/plain;charset=US-ASCII",
	55: "text/plain;charset=UTF-16",
	56: "text/plain;charset=UTF-8",
	57: "text/vcard",
	58: "video/H264",
	59: "video/H265",
	60: "video/H266",
	61: "video/ogg",
	62: "video/VP8",
	63: "video/VP9",
	64: "video/webm",
}

// mediaTypeToNumber is the reverse mapping built at init.
var mediaTypeToNumber map[string]uint8

func init() {
	mediaTypeToNumber = make(map[string]uint8, len(numberToMediaType))
	for num, s := range numberToMediaType {
		mediaTypeToNumber[s] = num
	}
}
