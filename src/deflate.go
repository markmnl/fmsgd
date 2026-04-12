package main

import (
	"compress/zlib"
	"io"
	"os"
	"strings"
)

// minDeflateSize is the minimum payload size in bytes before compression is
// attempted.
const minDeflateSize uint32 = 512

// incompressibleTypes lists media types (lowercased, without parameters) that
// are already compressed or otherwise unlikely to benefit from zlib-deflate.
var incompressibleTypes = map[string]bool{
	// images
	"image/jpeg": true, "image/png": true, "image/gif": true,
	"image/webp": true, "image/heic": true, "image/avif": true,
	"image/apng": true,
	// audio
	"audio/aac": true, "audio/mpeg": true, "audio/ogg": true,
	"audio/opus": true, "audio/webm": true,
	// video
	"video/h264": true, "video/h265": true, "video/h266": true,
	"video/ogg": true, "video/vp8": true, "video/vp9": true,
	"video/webm": true,
	// archives / compressed containers
	"application/gzip": true, "application/zip": true,
	"application/epub+zip":     true,
	"application/octet-stream": true,
	// zip-based office formats
	"application/vnd.oasis.opendocument.presentation":                           true,
	"application/vnd.oasis.opendocument.spreadsheet":                            true,
	"application/vnd.oasis.opendocument.text":                                   true,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         true,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   true,
	"application/vnd.amazon.ebook":                                              true,
	// fonts (compressed)
	"font/woff": true, "font/woff2": true,
	// pdf (internally compressed)
	"application/pdf": true,
	// 3d models (compressed containers)
	"model/3mf": true, "model/gltf-binary": true,
	"model/vnd.usdz+zip": true,
}

// shouldDeflate reports whether compression should be attempted for a payload
// with the given media type and size. It returns false for payloads that are
// too small or use a media type known to be already compressed.
func shouldDeflate(mediaType string, dataSize uint32) bool {
	if dataSize < minDeflateSize {
		return false
	}
	t := strings.ToLower(mediaType)
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimRight(t[:i], " ")
	}
	return !incompressibleTypes[t]
}

// tryDeflate compresses the file at srcPath using zlib-deflate and writes the
// result to a temporary file. It returns worthwhile=true only when the
// compressed output is less than 90% of the original size (at least a 10%
// reduction). When not worthwhile the temporary file is removed.
func tryDeflate(srcPath string, srcSize uint32) (dstPath string, compressedSize uint32, worthwhile bool, err error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", 0, false, err
	}
	defer src.Close()

	dst, err := os.CreateTemp("", "fmsg-deflate-*")
	if err != nil {
		return "", 0, false, err
	}
	dstName := dst.Name()

	zw := zlib.NewWriter(dst)
	if _, err := io.Copy(zw, src); err != nil {
		_ = zw.Close()
		_ = dst.Close()
		_ = os.Remove(dstName)
		return "", 0, false, err
	}
	if err := zw.Close(); err != nil {
		_ = dst.Close()
		_ = os.Remove(dstName)
		return "", 0, false, err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(dstName)
		return "", 0, false, err
	}

	fi, err := os.Stat(dstName)
	if err != nil {
		_ = os.Remove(dstName)
		return "", 0, false, err
	}

	cSize := uint32(fi.Size())
	if cSize >= srcSize*9/10 {
		_ = os.Remove(dstName)
		return "", 0, false, nil
	}

	return dstName, cSize, true, nil
}
