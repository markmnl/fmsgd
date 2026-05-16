package main

import (
	"bytes"
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

// shouldCompress reports whether compression should be attempted for a payload
// with the given media type and size. It returns false for payloads that are
// too small or use a media type known to be already compressed.
func shouldCompress(mediaType string, dataSize uint32) bool {
	if dataSize < minDeflateSize {
		return false
	}
	t := strings.ToLower(mediaType)
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimRight(t[:i], " ")
	}
	return !incompressibleTypes[t]
}

// deflateSampleSize is the number of bytes sampled from the start of a file
// to estimate compressibility before committing to a full-file compression
// pass. Chosen large enough for zlib to find patterns but small enough to be
// fast even on very large files.
const deflateSampleSize = 8192

// probeSample compresses up to deflateSampleSize bytes from the start of src
// and reports whether the ratio looks promising (compressed < 80% of input).
// src is seeked back to the start on return.
func probeSample(src *os.File, srcSize uint32) (bool, error) {
	sampleLen := int64(deflateSampleSize)
	if int64(srcSize) < sampleLen {
		sampleLen = int64(srcSize)
	}

	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := io.CopyN(zw, src, sampleLen); err != nil {
		_ = zw.Close()
		return false, err
	}
	if err := zw.Close(); err != nil {
		return false, err
	}

	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return false, err
	}

	return int64(buf.Len()) < sampleLen*8/10, nil
}

// tryCompress compresses the file at srcPath using zlib-deflate and writes the
// result to a temporary file. For files larger than deflateSampleSize it first
// compresses a prefix sample to estimate compressibility, avoiding a full pass
// over files that won't compress well. It returns worthwhile=true only when
// the compressed output is less than 80% of the original size (at least a 20%
// reduction). When not worthwhile the temporary file is removed. When
// worthwhile the caller is responsible for removing the file at dstPath.
func tryCompress(srcPath string, srcSize uint32) (dstPath string, compressedSize uint32, worthwhile bool, err error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", 0, false, err
	}
	defer src.Close()

	// For files larger than the sample size, probe a prefix first.
	if srcSize > deflateSampleSize {
		promising, err := probeSample(src, srcSize)
		if err != nil {
			return "", 0, false, err
		}
		if !promising {
			return "", 0, false, nil
		}
	}

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
	if cSize >= srcSize*8/10 {
		_ = os.Remove(dstName)
		return "", 0, false, nil
	}

	return dstName, cSize, true, nil
}
