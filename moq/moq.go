package moq

// #include <moq.h>
import "C"

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"reflect"
	"runtime"
	"runtime/cgo"
	"sync/atomic"
	"unsafe"
)

// This is needed, because as of go 1.24
// type RustBuffer C.RustBuffer cannot have methods,
// RustBuffer is treated as non-local type
type GoRustBuffer struct {
	inner C.RustBuffer
}

type RustBufferI interface {
	AsReader() *bytes.Reader
	Free()
	ToGoBytes() []byte
	Data() unsafe.Pointer
	Len() uint64
	Capacity() uint64
}

// C.RustBuffer fields exposed as an interface so they can be accessed in different Go packages.
// See https://github.com/golang/go/issues/13467
type ExternalCRustBuffer interface {
	Data() unsafe.Pointer
	Len() uint64
	Capacity() uint64
}

func RustBufferFromC(b C.RustBuffer) ExternalCRustBuffer {
	return GoRustBuffer{
		inner: b,
	}
}

func CFromRustBuffer(b ExternalCRustBuffer) C.RustBuffer {
	return C.RustBuffer{
		capacity: C.uint64_t(b.Capacity()),
		len:      C.uint64_t(b.Len()),
		data:     (*C.uchar)(b.Data()),
	}
}

func RustBufferFromExternal(b ExternalCRustBuffer) GoRustBuffer {
	return GoRustBuffer{
		inner: C.RustBuffer{
			capacity: C.uint64_t(b.Capacity()),
			len:      C.uint64_t(b.Len()),
			data:     (*C.uchar)(b.Data()),
		},
	}
}

func (cb GoRustBuffer) Capacity() uint64 {
	return uint64(cb.inner.capacity)
}

func (cb GoRustBuffer) Len() uint64 {
	return uint64(cb.inner.len)
}

func (cb GoRustBuffer) Data() unsafe.Pointer {
	return unsafe.Pointer(cb.inner.data)
}

func (cb GoRustBuffer) AsReader() *bytes.Reader {
	b := unsafe.Slice((*byte)(cb.inner.data), C.uint64_t(cb.inner.len))
	return bytes.NewReader(b)
}

func (cb GoRustBuffer) Free() {
	rustCall(func(status *C.RustCallStatus) bool {
		C.ffi_moq_ffi_rustbuffer_free(cb.inner, status)
		return false
	})
}

func (cb GoRustBuffer) ToGoBytes() []byte {
	return C.GoBytes(unsafe.Pointer(cb.inner.data), C.int(cb.inner.len))
}

func stringToRustBuffer(str string) C.RustBuffer {
	return bytesToRustBuffer([]byte(str))
}

func bytesToRustBuffer(b []byte) C.RustBuffer {
	if len(b) == 0 {
		return C.RustBuffer{}
	}
	// We can pass the pointer along here, as it is pinned
	// for the duration of this call
	foreign := C.ForeignBytes{
		len:  C.int(len(b)),
		data: (*C.uchar)(unsafe.Pointer(&b[0])),
	}

	return rustCall(func(status *C.RustCallStatus) C.RustBuffer {
		return C.ffi_moq_ffi_rustbuffer_from_bytes(foreign, status)
	})
}

type BufLifter[GoType any] interface {
	Lift(value RustBufferI) GoType
}

type BufLowerer[GoType any] interface {
	Lower(value GoType) C.RustBuffer
}

type BufReader[GoType any] interface {
	Read(reader io.Reader) GoType
}

type BufWriter[GoType any] interface {
	Write(writer io.Writer, value GoType)
}

func LowerIntoRustBuffer[GoType any](bufWriter BufWriter[GoType], value GoType) C.RustBuffer {
	// This might be not the most efficient way but it does not require knowing allocation size
	// beforehand
	var buffer bytes.Buffer
	bufWriter.Write(&buffer, value)

	bytes, err := io.ReadAll(&buffer)
	if err != nil {
		panic(fmt.Errorf("reading written data: %w", err))
	}
	return bytesToRustBuffer(bytes)
}

func LiftFromRustBuffer[GoType any](bufReader BufReader[GoType], rbuf RustBufferI) GoType {
	defer rbuf.Free()
	reader := rbuf.AsReader()
	item := bufReader.Read(reader)
	if reader.Len() > 0 {
		// TODO: Remove this
		leftover, _ := io.ReadAll(reader)
		panic(fmt.Errorf("Junk remaining in buffer after lifting: %s", string(leftover)))
	}
	return item
}

func rustCallWithError[E any, U any](converter BufReader[E], callback func(*C.RustCallStatus) U) (U, E) {
	var status C.RustCallStatus
	returnValue := callback(&status)
	err := checkCallStatus(converter, status)
	return returnValue, err
}

func checkCallStatus[E any](converter BufReader[E], status C.RustCallStatus) E {
	switch status.code {
	case 0:
		var zero E
		return zero
	case 1:
		return LiftFromRustBuffer(converter, GoRustBuffer{inner: status.errorBuf})
	case 2:
		// when the rust code sees a panic, it tries to construct a rustBuffer
		// with the message.  but if that code panics, then it just sends back
		// an empty buffer.
		if status.errorBuf.len > 0 {
			panic(fmt.Errorf("%s", FfiConverterStringINSTANCE.Lift(GoRustBuffer{inner: status.errorBuf})))
		} else {
			panic(fmt.Errorf("Rust panicked while handling Rust panic"))
		}
	default:
		panic(fmt.Errorf("unknown status code: %d", status.code))
	}
}

func checkCallStatusUnknown(status C.RustCallStatus) error {
	switch status.code {
	case 0:
		return nil
	case 1:
		panic(fmt.Errorf("function not returning an error returned an error"))
	case 2:
		// when the rust code sees a panic, it tries to construct a C.RustBuffer
		// with the message.  but if that code panics, then it just sends back
		// an empty buffer.
		if status.errorBuf.len > 0 {
			panic(fmt.Errorf("%s", FfiConverterStringINSTANCE.Lift(GoRustBuffer{
				inner: status.errorBuf,
			})))
		} else {
			panic(fmt.Errorf("Rust panicked while handling Rust panic"))
		}
	default:
		return fmt.Errorf("unknown status code: %d", status.code)
	}
}

func rustCall[U any](callback func(*C.RustCallStatus) U) U {
	returnValue, err := rustCallWithError[error](nil, callback)
	if err != nil {
		panic(err)
	}
	return returnValue
}

type NativeError interface {
	AsError() error
}

func writeInt8(writer io.Writer, value int8) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint8(writer io.Writer, value uint8) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt16(writer io.Writer, value int16) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint16(writer io.Writer, value uint16) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt32(writer io.Writer, value int32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint32(writer io.Writer, value uint32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt64(writer io.Writer, value int64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint64(writer io.Writer, value uint64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeFloat32(writer io.Writer, value float32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeFloat64(writer io.Writer, value float64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func readInt8(reader io.Reader) int8 {
	var result int8
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint8(reader io.Reader) uint8 {
	var result uint8
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt16(reader io.Reader) int16 {
	var result int16
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint16(reader io.Reader) uint16 {
	var result uint16
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt32(reader io.Reader) int32 {
	var result int32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint32(reader io.Reader) uint32 {
	var result uint32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt64(reader io.Reader) int64 {
	var result int64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint64(reader io.Reader) uint64 {
	var result uint64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readFloat32(reader io.Reader) float32 {
	var result float32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readFloat64(reader io.Reader) float64 {
	var result float64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func init() {

	uniffiCheckChecksums()
}

func uniffiCheckChecksums() {
	// Get the bindings contract version from our ComponentInterface
	bindingsContractVersion := 30
	// Get the scaffolding contract version by calling the into the dylib
	scaffoldingContractVersion := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint32_t {
		return C.ffi_moq_ffi_uniffi_contract_version()
	})
	if bindingsContractVersion != int(scaffoldingContractVersion) {
		// If this happens try cleaning and rebuilding your project
		panic("moq: UniFFI contract version mismatch")
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_func_moq_log_level()
		})
		if checksum != 27140 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_func_moq_log_level: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqaudioconsumer_cancel()
		})
		if checksum != 33004 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqaudioconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqaudioconsumer_next()
		})
		if checksum != 55387 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqaudioconsumer_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqaudioproducer_finish()
		})
		if checksum != 41749 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqaudioproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqaudioproducer_write()
		})
		if checksum != 49517 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqaudioproducer_write: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_audio()
		})
		if checksum != 50677 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_audio: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_fetch_group()
		})
		if checksum != 28258 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_fetch_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_route()
		})
		if checksum != 16738 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_route: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_route_updates()
		})
		if checksum != 5829 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_route_updates: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_catalog()
		})
		if checksum != 47602 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_catalog: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_media()
		})
		if checksum != 1303 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_media: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_track()
		})
		if checksum != 16615 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_track: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_json_snapshot()
		})
		if checksum != 29757 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_json_snapshot: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_json_stream()
		})
		if checksum != 53781 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastconsumer_subscribe_json_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcatalogconsumer_cancel()
		})
		if checksum != 1059 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcatalogconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqcatalogconsumer_next()
		})
		if checksum != 42881 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqcatalogconsumer_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupconsumer_cancel()
		})
		if checksum != 21782 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupconsumer_read_frame()
		})
		if checksum != 43591 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupconsumer_read_frame: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupconsumer_sequence()
		})
		if checksum != 61070 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupconsumer_sequence: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaconsumer_cancel()
		})
		if checksum != 12542 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaconsumer_next()
		})
		if checksum != 49285 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaconsumer_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqroutewatch_cancel()
		})
		if checksum != 61300 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqroutewatch_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqroutewatch_next()
		})
		if checksum != 34673 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqroutewatch_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackconsumer_cancel()
		})
		if checksum != 13373 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackconsumer_info()
		})
		if checksum != 42913 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackconsumer_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackconsumer_next_group()
		})
		if checksum != 38789 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackconsumer_next_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackconsumer_read_frame()
		})
		if checksum != 13112 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackconsumer_read_frame: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackconsumer_recv_datagram()
		})
		if checksum != 58393 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackconsumer_recv_datagram: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackconsumer_recv_group()
		})
		if checksum != 26719 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackconsumer_recv_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackconsumer_update()
		})
		if checksum != 37123 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackconsumer_update: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonsnapshotconsumer_cancel()
		})
		if checksum != 20318 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonsnapshotconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonsnapshotconsumer_next()
		})
		if checksum != 40200 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonsnapshotconsumer_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonsnapshotproducer_finish()
		})
		if checksum != 44519 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonsnapshotproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonsnapshotproducer_update()
		})
		if checksum != 45946 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonsnapshotproducer_update: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonstreamconsumer_cancel()
		})
		if checksum != 60362 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonstreamconsumer_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonstreamconsumer_next()
		})
		if checksum != 30879 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonstreamconsumer_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonstreamproducer_append()
		})
		if checksum != 29352 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonstreamproducer_append: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqjsonstreamproducer_finish()
		})
		if checksum != 52088 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqjsonstreamproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqannounced_cancel()
		})
		if checksum != 11787 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqannounced_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqannounced_next()
		})
		if checksum != 30814 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqannounced_next: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqannouncedbroadcast_available()
		})
		if checksum != 46046 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqannouncedbroadcast_available: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqannouncedbroadcast_cancel()
		})
		if checksum != 63780 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqannouncedbroadcast_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqannouncement_broadcast()
		})
		if checksum != 8318 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqannouncement_broadcast: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqannouncement_path()
		})
		if checksum != 33642 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqannouncement_path: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastrequest_abort()
		})
		if checksum != 38615 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastrequest_abort: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastrequest_accept()
		})
		if checksum != 4831 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastrequest_accept: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastrequest_path()
		})
		if checksum != 28555 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastrequest_path: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqoriginconsumer_announced()
		})
		if checksum != 65430 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqoriginconsumer_announced: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqoriginconsumer_announced_broadcast()
		})
		if checksum != 54838 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqoriginconsumer_announced_broadcast: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqoriginconsumer_request_broadcast()
		})
		if checksum != 63880 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqoriginconsumer_request_broadcast: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqorigindynamic_cancel()
		})
		if checksum != 55027 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqorigindynamic_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqorigindynamic_requested_broadcast()
		})
		if checksum != 53494 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqorigindynamic_requested_broadcast: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqoriginproducer_consume()
		})
		if checksum != 34292 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqoriginproducer_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqoriginproducer_create_broadcast()
		})
		if checksum != 7635 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqoriginproducer_create_broadcast: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqoriginproducer_dynamic()
		})
		if checksum != 19783 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqoriginproducer_dynamic: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastdynamic_cancel()
		})
		if checksum != 41601 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastdynamic_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastdynamic_requested_track()
		})
		if checksum != 43684 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastdynamic_requested_track: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_audio()
		})
		if checksum != 39786 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_audio: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_json_snapshot()
		})
		if checksum != 53223 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_json_snapshot: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_json_stream()
		})
		if checksum != 37537 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_json_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_consume()
		})
		if checksum != 46595 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_dynamic()
		})
		if checksum != 46433 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_dynamic: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_finish()
		})
		if checksum != 9168 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_media()
		})
		if checksum != 44712 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_media: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_media_on_track()
		})
		if checksum != 61309 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_media_on_track: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_media_stream()
		})
		if checksum != 25992 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_media_stream: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_track()
		})
		if checksum != 41835 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_publish_track: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_remove_catalog_section()
		})
		if checksum != 42326 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_remove_catalog_section: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_set_announce()
		})
		if checksum != 18686 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_set_announce: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_set_catalog_section()
		})
		if checksum != 8811 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_set_catalog_section: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_set_route()
		})
		if checksum != 2362 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_set_route: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqbroadcastproducer_set_video_properties()
		})
		if checksum != 30609 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqbroadcastproducer_set_video_properties: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupproducer_abort()
		})
		if checksum != 22408 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupproducer_abort: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupproducer_consume()
		})
		if checksum != 12315 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupproducer_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupproducer_finish()
		})
		if checksum != 39760 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupproducer_sequence()
		})
		if checksum != 11821 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupproducer_sequence: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgroupproducer_write_frame()
		})
		if checksum != 42697 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgroupproducer_write_frame: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgrouprequest_abort()
		})
		if checksum != 53199 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgrouprequest_abort: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgrouprequest_accept()
		})
		if checksum != 38986 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgrouprequest_accept: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgrouprequest_priority()
		})
		if checksum != 30080 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgrouprequest_priority: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqgrouprequest_sequence()
		})
		if checksum != 34701 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqgrouprequest_sequence: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_finish()
		})
		if checksum != 13508 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_name()
		})
		if checksum != 35932 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_name: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_unused()
		})
		if checksum != 65253 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_unused: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_used()
		})
		if checksum != 29546 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_used: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediaproducer_write_frame()
		})
		if checksum != 46299 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediaproducer_write_frame: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediastreamproducer_finish()
		})
		if checksum != 44939 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediastreamproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqmediastreamproducer_write()
		})
		if checksum != 47083 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqmediastreamproducer_write: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackdynamic_cancel()
		})
		if checksum != 7164 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackdynamic_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackdynamic_requested_group()
		})
		if checksum != 6073 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackdynamic_requested_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_abort()
		})
		if checksum != 46232 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_abort: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_append_datagram()
		})
		if checksum != 35955 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_append_datagram: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_append_group()
		})
		if checksum != 28433 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_append_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_consume()
		})
		if checksum != 23920 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_create_group()
		})
		if checksum != 8039 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_create_group: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_dynamic()
		})
		if checksum != 30887 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_dynamic: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_finish()
		})
		if checksum != 8455 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_finish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_finish_at()
		})
		if checksum != 55850 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_finish_at: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_name()
		})
		if checksum != 18320 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_name: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_unused()
		})
		if checksum != 40969 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_unused: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_used()
		})
		if checksum != 20539 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_used: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackproducer_write_frame()
		})
		if checksum != 61798 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackproducer_write_frame: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackrequest_abort()
		})
		if checksum != 37864 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackrequest_abort: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackrequest_accept()
		})
		if checksum != 59540 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackrequest_accept: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackrequest_dynamic()
		})
		if checksum != 36895 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackrequest_dynamic: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqtrackrequest_name()
		})
		if checksum != 15932 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqtrackrequest_name: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_accept()
		})
		if checksum != 35301 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_accept: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_cancel()
		})
		if checksum != 63846 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_reject()
		})
		if checksum != 28918 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_reject: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_set_consume()
		})
		if checksum != 25024 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_set_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_set_publish()
		})
		if checksum != 5686 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_set_publish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_transport()
		})
		if checksum != 789 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_transport: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqrequest_url()
		})
		if checksum != 34738 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqrequest_url: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_accept()
		})
		if checksum != 41383 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_accept: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_cancel()
		})
		if checksum != 36526 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_cert_fingerprints()
		})
		if checksum != 38274 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_cert_fingerprints: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_listen()
		})
		if checksum != 19779 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_listen: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_set_bind()
		})
		if checksum != 53276 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_set_bind: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_set_consume()
		})
		if checksum != 10795 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_set_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_set_publish()
		})
		if checksum != 48707 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_set_publish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_set_tls_cert()
		})
		if checksum != 59890 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_set_tls_cert: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_set_tls_generate()
		})
		if checksum != 42920 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_set_tls_generate: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqserver_set_tls_key()
		})
		if checksum != 43796 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqserver_set_tls_key: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_cancel()
		})
		if checksum != 42343 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_connect()
		})
		if checksum != 57228 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_connect: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_bind()
		})
		if checksum != 42107 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_bind: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_consume()
		})
		if checksum != 55200 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_consume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_publish()
		})
		if checksum != 56893 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_publish: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_tls_cert()
		})
		if checksum != 45194 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_tls_cert: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_tls_disable_verify()
		})
		if checksum != 17397 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_tls_disable_verify: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_tls_fingerprints()
		})
		if checksum != 55328 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_tls_fingerprints: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_tls_key()
		})
		if checksum != 13628 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_tls_key: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_tls_roots()
		})
		if checksum != 54966 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_tls_roots: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqclient_set_tls_system_roots()
		})
		if checksum != 42515 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqclient_set_tls_system_roots: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_cancel()
		})
		if checksum != 24930 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_cancel: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_closed()
		})
		if checksum != 41657 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_closed: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_consumer()
		})
		if checksum != 40339 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_consumer: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_publisher()
		})
		if checksum != 24627 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_publisher: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_shutdown()
		})
		if checksum != 15895 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_shutdown: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_method_moqsession_stats()
		})
		if checksum != 26506 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_method_moqsession_stats: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_constructor_moqoriginproducer_new()
		})
		if checksum != 58041 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_constructor_moqoriginproducer_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_constructor_moqbroadcastproducer_new()
		})
		if checksum != 48518 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_constructor_moqbroadcastproducer_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_constructor_moqserver_new()
		})
		if checksum != 36783 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_constructor_moqserver_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_moq_ffi_checksum_constructor_moqclient_new()
		})
		if checksum != 62327 {
			// If this happens try cleaning and rebuilding your project
			panic("moq: uniffi_moq_ffi_checksum_constructor_moqclient_new: UniFFI API checksum mismatch")
		}
	}
}

type FfiConverterUint8 struct{}

var FfiConverterUint8INSTANCE = FfiConverterUint8{}

func (FfiConverterUint8) Lower(value uint8) C.uint8_t {
	return C.uint8_t(value)
}

func (FfiConverterUint8) Write(writer io.Writer, value uint8) {
	writeUint8(writer, value)
}

func (FfiConverterUint8) Lift(value C.uint8_t) uint8 {
	return uint8(value)
}

func (FfiConverterUint8) Read(reader io.Reader) uint8 {
	return readUint8(reader)
}

type FfiDestroyerUint8 struct{}

func (FfiDestroyerUint8) Destroy(_ uint8) {}

type FfiConverterUint16 struct{}

var FfiConverterUint16INSTANCE = FfiConverterUint16{}

func (FfiConverterUint16) Lower(value uint16) C.uint16_t {
	return C.uint16_t(value)
}

func (FfiConverterUint16) Write(writer io.Writer, value uint16) {
	writeUint16(writer, value)
}

func (FfiConverterUint16) Lift(value C.uint16_t) uint16 {
	return uint16(value)
}

func (FfiConverterUint16) Read(reader io.Reader) uint16 {
	return readUint16(reader)
}

type FfiDestroyerUint16 struct{}

func (FfiDestroyerUint16) Destroy(_ uint16) {}

type FfiConverterUint32 struct{}

var FfiConverterUint32INSTANCE = FfiConverterUint32{}

func (FfiConverterUint32) Lower(value uint32) C.uint32_t {
	return C.uint32_t(value)
}

func (FfiConverterUint32) Write(writer io.Writer, value uint32) {
	writeUint32(writer, value)
}

func (FfiConverterUint32) Lift(value C.uint32_t) uint32 {
	return uint32(value)
}

func (FfiConverterUint32) Read(reader io.Reader) uint32 {
	return readUint32(reader)
}

type FfiDestroyerUint32 struct{}

func (FfiDestroyerUint32) Destroy(_ uint32) {}

type FfiConverterUint64 struct{}

var FfiConverterUint64INSTANCE = FfiConverterUint64{}

func (FfiConverterUint64) Lower(value uint64) C.uint64_t {
	return C.uint64_t(value)
}

func (FfiConverterUint64) Write(writer io.Writer, value uint64) {
	writeUint64(writer, value)
}

func (FfiConverterUint64) Lift(value C.uint64_t) uint64 {
	return uint64(value)
}

func (FfiConverterUint64) Read(reader io.Reader) uint64 {
	return readUint64(reader)
}

type FfiDestroyerUint64 struct{}

func (FfiDestroyerUint64) Destroy(_ uint64) {}

type FfiConverterFloat64 struct{}

var FfiConverterFloat64INSTANCE = FfiConverterFloat64{}

func (FfiConverterFloat64) Lower(value float64) C.double {
	return C.double(value)
}

func (FfiConverterFloat64) Write(writer io.Writer, value float64) {
	writeFloat64(writer, value)
}

func (FfiConverterFloat64) Lift(value C.double) float64 {
	return float64(value)
}

func (FfiConverterFloat64) Read(reader io.Reader) float64 {
	return readFloat64(reader)
}

type FfiDestroyerFloat64 struct{}

func (FfiDestroyerFloat64) Destroy(_ float64) {}

type FfiConverterBool struct{}

var FfiConverterBoolINSTANCE = FfiConverterBool{}

func (FfiConverterBool) Lower(value bool) C.int8_t {
	if value {
		return C.int8_t(1)
	}
	return C.int8_t(0)
}

func (FfiConverterBool) Write(writer io.Writer, value bool) {
	if value {
		writeInt8(writer, 1)
	} else {
		writeInt8(writer, 0)
	}
}

func (FfiConverterBool) Lift(value C.int8_t) bool {
	return value != 0
}

func (FfiConverterBool) Read(reader io.Reader) bool {
	return readInt8(reader) != 0
}

type FfiDestroyerBool struct{}

func (FfiDestroyerBool) Destroy(_ bool) {}

type FfiConverterString struct{}

var FfiConverterStringINSTANCE = FfiConverterString{}

func (FfiConverterString) Lift(rb RustBufferI) string {
	defer rb.Free()
	reader := rb.AsReader()
	b, err := io.ReadAll(reader)
	if err != nil {
		panic(fmt.Errorf("reading reader: %w", err))
	}
	return string(b)
}

func (FfiConverterString) Read(reader io.Reader) string {
	length := readInt32(reader)
	buffer := make([]byte, length)
	read_length, err := reader.Read(buffer)
	if err != nil && err != io.EOF {
		panic(err)
	}
	if read_length != int(length) {
		panic(fmt.Errorf("bad read length when reading string, expected %d, read %d", length, read_length))
	}
	return string(buffer)
}

func (FfiConverterString) Lower(value string) C.RustBuffer {
	return stringToRustBuffer(value)
}

func (c FfiConverterString) LowerExternal(value string) ExternalCRustBuffer {
	return RustBufferFromC(stringToRustBuffer(value))
}

func (FfiConverterString) Write(writer io.Writer, value string) {
	if len(value) > math.MaxInt32 {
		panic("String is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	write_length, err := io.WriteString(writer, value)
	if err != nil {
		panic(err)
	}
	if write_length != len(value) {
		panic(fmt.Errorf("bad write length when writing string, expected %d, written %d", len(value), write_length))
	}
}

type FfiDestroyerString struct{}

func (FfiDestroyerString) Destroy(_ string) {}

type FfiConverterBytes struct{}

var FfiConverterBytesINSTANCE = FfiConverterBytes{}

func (c FfiConverterBytes) Lower(value []byte) C.RustBuffer {
	return LowerIntoRustBuffer[[]byte](c, value)
}

func (c FfiConverterBytes) LowerExternal(value []byte) ExternalCRustBuffer {
	return RustBufferFromC(c.Lower(value))
}

func (c FfiConverterBytes) Write(writer io.Writer, value []byte) {
	if len(value) > math.MaxInt32 {
		panic("[]byte is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	write_length, err := writer.Write(value)
	if err != nil {
		panic(err)
	}
	if write_length != len(value) {
		panic(fmt.Errorf("bad write length when writing []byte, expected %d, written %d", len(value), write_length))
	}
}

func (c FfiConverterBytes) Lift(rb RustBufferI) []byte {
	return LiftFromRustBuffer[[]byte](c, rb)
}

func (c FfiConverterBytes) Read(reader io.Reader) []byte {
	length := readInt32(reader)
	buffer := make([]byte, length)
	read_length, err := reader.Read(buffer)
	if err != nil && err != io.EOF {
		panic(err)
	}
	if read_length != int(length) {
		panic(fmt.Errorf("bad read length when reading []byte, expected %d, read %d", length, read_length))
	}
	return buffer
}

type FfiDestroyerBytes struct{}

func (FfiDestroyerBytes) Destroy(_ []byte) {}

// Below is an implementation of synchronization requirements outlined in the link.
// https://github.com/mozilla/uniffi-rs/blob/0dc031132d9493ca812c3af6e7dd60ad2ea95bf0/uniffi_bindgen/src/bindings/kotlin/templates/ObjectRuntime.kt#L31

type FfiObject struct {
	handle        C.uint64_t
	callCounter   atomic.Int64
	cloneFunction func(C.uint64_t, *C.RustCallStatus) C.uint64_t
	freeFunction  func(C.uint64_t, *C.RustCallStatus)
	destroyed     atomic.Bool
}

func newFfiObject(
	handle C.uint64_t,
	cloneFunction func(C.uint64_t, *C.RustCallStatus) C.uint64_t,
	freeFunction func(C.uint64_t, *C.RustCallStatus),
) FfiObject {
	return FfiObject{
		handle:        handle,
		cloneFunction: cloneFunction,
		freeFunction:  freeFunction,
	}
}

func (ffiObject *FfiObject) incrementPointer(debugName string) C.uint64_t {
	for {
		counter := ffiObject.callCounter.Load()
		if counter <= -1 {
			panic(fmt.Errorf("%v object has already been destroyed", debugName))
		}
		if counter == math.MaxInt64 {
			panic(fmt.Errorf("%v object call counter would overflow", debugName))
		}
		if ffiObject.callCounter.CompareAndSwap(counter, counter+1) {
			break
		}
	}

	return rustCall(func(status *C.RustCallStatus) C.uint64_t {
		return ffiObject.cloneFunction(ffiObject.handle, status)
	})
}

func (ffiObject *FfiObject) decrementPointer() {
	if ffiObject.callCounter.Add(-1) == -1 {
		ffiObject.freeRustArcPtr()
	}
}

func (ffiObject *FfiObject) destroy() {
	if ffiObject.destroyed.CompareAndSwap(false, true) {
		if ffiObject.callCounter.Add(-1) == -1 {
			ffiObject.freeRustArcPtr()
		}
	}
}

func (ffiObject *FfiObject) freeRustArcPtr() {
	if ffiObject.handle == 0 {
		return
	}
	rustCall(func(status *C.RustCallStatus) int32 {
		ffiObject.freeFunction(ffiObject.handle, status)
		return 0
	})
}

type MoqAnnouncedInterface interface {
	// Cancel all current and future `next()` calls.
	Cancel()
	// Get the next broadcast announcement. Returns `None` when the origin is closed.
	//
	// Use `broadcast.closed()` to learn when a broadcast is unannounced.
	Next() (**MoqAnnouncement, error)
}
type MoqAnnounced struct {
	ffiObject FfiObject
}

// Cancel all current and future `next()` calls.
func (_self *MoqAnnounced) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqAnnounced")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqannounced_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Get the next broadcast announcement. Returns `None` when the origin is closed.
//
// Use `broadcast.closed()` to learn when a broadcast is unannounced.
func (_self *MoqAnnounced) Next() (**MoqAnnouncement, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqAnnounced")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) **MoqAnnouncement {
			return FfiConverterOptionalMoqAnnouncementINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqannounced_next(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *MoqAnnounced) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqAnnounced struct{}

var FfiConverterMoqAnnouncedINSTANCE = FfiConverterMoqAnnounced{}

func (c FfiConverterMoqAnnounced) Lift(handle C.uint64_t) *MoqAnnounced {
	result := &MoqAnnounced{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqannounced(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqannounced(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqAnnounced).Destroy)
	return result
}

func (c FfiConverterMoqAnnounced) Read(reader io.Reader) *MoqAnnounced {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqAnnounced) Lower(value *MoqAnnounced) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqAnnounced")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqAnnounced) Write(writer io.Writer, value *MoqAnnounced) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqAnnounced(handle uint64) *MoqAnnounced {
	return FfiConverterMoqAnnouncedINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqAnnounced(value *MoqAnnounced) uint64 {
	return uint64(FfiConverterMoqAnnouncedINSTANCE.Lower(value))
}

type FfiDestroyerMoqAnnounced struct{}

func (_ FfiDestroyerMoqAnnounced) Destroy(value *MoqAnnounced) {
	value.Destroy()
}

// Waits for a specific broadcast to be announced.
type MoqAnnouncedBroadcastInterface interface {
	// Wait until the broadcast is announced. Returns `Closed` if cancelled or the origin is closed.
	//
	// Use `broadcast.closed()` to learn when a broadcast is unannounced.
	Available() (*MoqBroadcastConsumer, error)
	// Cancel all current and future `available()` calls.
	Cancel()
}

// Waits for a specific broadcast to be announced.
type MoqAnnouncedBroadcast struct {
	ffiObject FfiObject
}

// Wait until the broadcast is announced. Returns `Closed` if cancelled or the origin is closed.
//
// Use `broadcast.closed()` to learn when a broadcast is unannounced.
func (_self *MoqAnnouncedBroadcast) Available() (*MoqBroadcastConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqAnnouncedBroadcast")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqBroadcastConsumer {
			return FfiConverterMoqBroadcastConsumerINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqannouncedbroadcast_available(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Cancel all current and future `available()` calls.
func (_self *MoqAnnouncedBroadcast) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqAnnouncedBroadcast")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqannouncedbroadcast_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}
func (object *MoqAnnouncedBroadcast) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqAnnouncedBroadcast struct{}

var FfiConverterMoqAnnouncedBroadcastINSTANCE = FfiConverterMoqAnnouncedBroadcast{}

func (c FfiConverterMoqAnnouncedBroadcast) Lift(handle C.uint64_t) *MoqAnnouncedBroadcast {
	result := &MoqAnnouncedBroadcast{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqannouncedbroadcast(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqannouncedbroadcast(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqAnnouncedBroadcast).Destroy)
	return result
}

func (c FfiConverterMoqAnnouncedBroadcast) Read(reader io.Reader) *MoqAnnouncedBroadcast {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqAnnouncedBroadcast) Lower(value *MoqAnnouncedBroadcast) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqAnnouncedBroadcast")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqAnnouncedBroadcast) Write(writer io.Writer, value *MoqAnnouncedBroadcast) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqAnnouncedBroadcast(handle uint64) *MoqAnnouncedBroadcast {
	return FfiConverterMoqAnnouncedBroadcastINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqAnnouncedBroadcast(value *MoqAnnouncedBroadcast) uint64 {
	return uint64(FfiConverterMoqAnnouncedBroadcastINSTANCE.Lower(value))
}

type FfiDestroyerMoqAnnouncedBroadcast struct{}

func (_ FfiDestroyerMoqAnnouncedBroadcast) Destroy(value *MoqAnnouncedBroadcast) {
	value.Destroy()
}

// A broadcast announcement from an origin.
type MoqAnnouncementInterface interface {
	// The broadcast consumer.
	Broadcast() *MoqBroadcastConsumer
	// The path of the announced broadcast.
	Path() string
}

// A broadcast announcement from an origin.
type MoqAnnouncement struct {
	ffiObject FfiObject
}

// The broadcast consumer.
func (_self *MoqAnnouncement) Broadcast() *MoqBroadcastConsumer {
	_pointer := _self.ffiObject.incrementPointer("*MoqAnnouncement")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqBroadcastConsumerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqannouncement_broadcast(
			_pointer, _uniffiStatus)
	}))
}

// The path of the announced broadcast.
func (_self *MoqAnnouncement) Path() string {
	_pointer := _self.ffiObject.incrementPointer("*MoqAnnouncement")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqannouncement_path(
				_pointer, _uniffiStatus),
		}
	}))
}
func (object *MoqAnnouncement) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqAnnouncement struct{}

var FfiConverterMoqAnnouncementINSTANCE = FfiConverterMoqAnnouncement{}

func (c FfiConverterMoqAnnouncement) Lift(handle C.uint64_t) *MoqAnnouncement {
	result := &MoqAnnouncement{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqannouncement(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqannouncement(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqAnnouncement).Destroy)
	return result
}

func (c FfiConverterMoqAnnouncement) Read(reader io.Reader) *MoqAnnouncement {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqAnnouncement) Lower(value *MoqAnnouncement) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqAnnouncement")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqAnnouncement) Write(writer io.Writer, value *MoqAnnouncement) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqAnnouncement(handle uint64) *MoqAnnouncement {
	return FfiConverterMoqAnnouncementINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqAnnouncement(value *MoqAnnouncement) uint64 {
	return uint64(FfiConverterMoqAnnouncementINSTANCE.Lower(value))
}

type FfiDestroyerMoqAnnouncement struct{}

func (_ FfiDestroyerMoqAnnouncement) Destroy(value *MoqAnnouncement) {
	value.Destroy()
}

// Consumer for a raw-audio track.
type MoqAudioConsumerInterface interface {
	Cancel()
	Next() (*MoqAudioFrame, error)
}

// Consumer for a raw-audio track.
type MoqAudioConsumer struct {
	ffiObject FfiObject
}

func (_self *MoqAudioConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqAudioConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqaudioconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

func (_self *MoqAudioConsumer) Next() (*MoqAudioFrame, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqAudioConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqAudioFrame {
			return FfiConverterOptionalMoqAudioFrameINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqaudioconsumer_next(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *MoqAudioConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqAudioConsumer struct{}

var FfiConverterMoqAudioConsumerINSTANCE = FfiConverterMoqAudioConsumer{}

func (c FfiConverterMoqAudioConsumer) Lift(handle C.uint64_t) *MoqAudioConsumer {
	result := &MoqAudioConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqaudioconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqaudioconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqAudioConsumer).Destroy)
	return result
}

func (c FfiConverterMoqAudioConsumer) Read(reader io.Reader) *MoqAudioConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqAudioConsumer) Lower(value *MoqAudioConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqAudioConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqAudioConsumer) Write(writer io.Writer, value *MoqAudioConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqAudioConsumer(handle uint64) *MoqAudioConsumer {
	return FfiConverterMoqAudioConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqAudioConsumer(value *MoqAudioConsumer) uint64 {
	return uint64(FfiConverterMoqAudioConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqAudioConsumer struct{}

func (_ FfiDestroyerMoqAudioConsumer) Destroy(value *MoqAudioConsumer) {
	value.Destroy()
}

// Producer for a raw-audio track.
//
// Built via [`MoqBroadcastProducer::publish_audio`]. Each
// [`write`](Self::write) accepts an [`MoqAudioFrame`] whose `data`
// is PCM in the format declared by the [`MoqAudioEncoderInput`]
// passed at publish time.
type MoqAudioProducerInterface interface {
	Finish() error
	Write(frame MoqAudioFrame) error
}

// Producer for a raw-audio track.
//
// Built via [`MoqBroadcastProducer::publish_audio`]. Each
// [`write`](Self::write) accepts an [`MoqAudioFrame`] whose `data`
// is PCM in the format declared by the [`MoqAudioEncoderInput`]
// passed at publish time.
type MoqAudioProducer struct {
	ffiObject FfiObject
}

func (_self *MoqAudioProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqAudioProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqaudioproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

func (_self *MoqAudioProducer) Write(frame MoqAudioFrame) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqAudioProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqaudioproducer_write(
			_pointer, FfiConverterMoqAudioFrameINSTANCE.Lower(frame), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqAudioProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqAudioProducer struct{}

var FfiConverterMoqAudioProducerINSTANCE = FfiConverterMoqAudioProducer{}

func (c FfiConverterMoqAudioProducer) Lift(handle C.uint64_t) *MoqAudioProducer {
	result := &MoqAudioProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqaudioproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqaudioproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqAudioProducer).Destroy)
	return result
}

func (c FfiConverterMoqAudioProducer) Read(reader io.Reader) *MoqAudioProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqAudioProducer) Lower(value *MoqAudioProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqAudioProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqAudioProducer) Write(writer io.Writer, value *MoqAudioProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqAudioProducer(handle uint64) *MoqAudioProducer {
	return FfiConverterMoqAudioProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqAudioProducer(value *MoqAudioProducer) uint64 {
	return uint64(FfiConverterMoqAudioProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqAudioProducer struct{}

func (_ FfiDestroyerMoqAudioProducer) Destroy(value *MoqAudioProducer) {
	value.Destroy()
}

type MoqBroadcastConsumerInterface interface {
	// Subscribe to an audio track. `catalog_audio_config` comes from
	// the catalog (see
	// [`MoqCatalogConsumer::next`](crate::consumer::MoqCatalogConsumer::next));
	// the codec is inferred from it. Only Opus is currently supported.
	SubscribeAudio(name string, catalogAudio MoqAudio, output MoqAudioDecoderOutput) (*MoqAudioConsumer, error)
	// Fetch one complete group by track name and group sequence.
	//
	// This does not create a live subscription. A retained group resolves immediately;
	// otherwise the request waits for a dynamic producer to serve it. The returned
	// group may still be in progress, so read frames until `read_frame()` returns `None`.
	FetchGroup(name string, sequence uint64, options *MoqFetchGroupOptions) (*MoqGroupConsumer, error)
	// The route the broadcast currently takes to reach this origin.
	Route() MoqRoute
	// Watch the broadcast's route for changes.
	//
	// The returned watch yields the current route first, then every update
	// (e.g. an upstream failover), so a loop observes the full history from now.
	RouteUpdates() *MoqRouteWatch
	// Subscribe to the catalog for this broadcast.
	SubscribeCatalog() (*MoqCatalogConsumer, error)
	// Subscribe to a track by name, delivering frames in decode order.
	//
	// `container` is the track container from the catalog.
	// `subscription` tunes delivery priority, group ordering priority, and group range; omit for defaults.
	//
	// [`MoqSubscription::latency_max_ms`] bounds the local jitter buffer as well as
	// the publisher's cache, so both ends skip a stalled group on the same budget.
	SubscribeMedia(name string, container MoqContainer, subscription *MoqSubscription) (*MoqMediaConsumer, error)
	// Subscribe to a track by name, the same pattern as moq-boy's command/status tracks.
	//
	// Frames are returned as plain byte payloads with no codec or container parsing.
	// `subscription` tunes delivery priority, group ordering priority, and group range; omit for defaults.
	SubscribeTrack(name string, subscription *MoqSubscription) (*MoqTrackConsumer, error)
	// Subscribe to a JSON snapshot track (lossy latest-value) by name.
	//
	// Pass the same [`MoqJsonSnapshotConfig::compression`] the producer used.
	SubscribeJsonSnapshot(name string, config MoqJsonSnapshotConfig) (*MoqJsonSnapshotConsumer, error)
	// Subscribe to a JSON stream track (lossless append-log) by name.
	SubscribeJsonStream(name string, config MoqJsonStreamConfig) (*MoqJsonStreamConsumer, error)
}
type MoqBroadcastConsumer struct {
	ffiObject FfiObject
}

// Subscribe to an audio track. `catalog_audio_config` comes from
// the catalog (see
// [`MoqCatalogConsumer::next`](crate::consumer::MoqCatalogConsumer::next));
// the codec is inferred from it. Only Opus is currently supported.
func (_self *MoqBroadcastConsumer) SubscribeAudio(name string, catalogAudio MoqAudio, output MoqAudioDecoderOutput) (*MoqAudioConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqAudioConsumer {
			return FfiConverterMoqAudioConsumerINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_subscribe_audio(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqAudioINSTANCE.Lower(catalogAudio), FfiConverterMoqAudioDecoderOutputINSTANCE.Lower(output)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Fetch one complete group by track name and group sequence.
//
// This does not create a live subscription. A retained group resolves immediately;
// otherwise the request waits for a dynamic producer to serve it. The returned
// group may still be in progress, so read frames until `read_frame()` returns `None`.
func (_self *MoqBroadcastConsumer) FetchGroup(name string, sequence uint64, options *MoqFetchGroupOptions) (*MoqGroupConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqGroupConsumer {
			return FfiConverterMoqGroupConsumerINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_fetch_group(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterUint64INSTANCE.Lower(sequence), FfiConverterOptionalMoqFetchGroupOptionsINSTANCE.Lower(options)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// The route the broadcast currently takes to reach this origin.
func (_self *MoqBroadcastConsumer) Route() MoqRoute {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqRouteINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_route(
				_pointer, _uniffiStatus),
		}
	}))
}

// Watch the broadcast's route for changes.
//
// The returned watch yields the current route first, then every update
// (e.g. an upstream failover), so a loop observes the full history from now.
func (_self *MoqBroadcastConsumer) RouteUpdates() *MoqRouteWatch {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqRouteWatchINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_route_updates(
			_pointer, _uniffiStatus)
	}))
}

// Subscribe to the catalog for this broadcast.
func (_self *MoqBroadcastConsumer) SubscribeCatalog() (*MoqCatalogConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqCatalogConsumer {
			return FfiConverterMoqCatalogConsumerINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_subscribe_catalog(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Subscribe to a track by name, delivering frames in decode order.
//
// `container` is the track container from the catalog.
// `subscription` tunes delivery priority, group ordering priority, and group range; omit for defaults.
//
// [`MoqSubscription::latency_max_ms`] bounds the local jitter buffer as well as
// the publisher's cache, so both ends skip a stalled group on the same budget.
func (_self *MoqBroadcastConsumer) SubscribeMedia(name string, container MoqContainer, subscription *MoqSubscription) (*MoqMediaConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqMediaConsumer {
			return FfiConverterMoqMediaConsumerINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_subscribe_media(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqContainerINSTANCE.Lower(container), FfiConverterOptionalMoqSubscriptionINSTANCE.Lower(subscription)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Subscribe to a track by name, the same pattern as moq-boy's command/status tracks.
//
// Frames are returned as plain byte payloads with no codec or container parsing.
// `subscription` tunes delivery priority, group ordering priority, and group range; omit for defaults.
func (_self *MoqBroadcastConsumer) SubscribeTrack(name string, subscription *MoqSubscription) (*MoqTrackConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqTrackConsumer {
			return FfiConverterMoqTrackConsumerINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_subscribe_track(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterOptionalMoqSubscriptionINSTANCE.Lower(subscription)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Subscribe to a JSON snapshot track (lossy latest-value) by name.
//
// Pass the same [`MoqJsonSnapshotConfig::compression`] the producer used.
func (_self *MoqBroadcastConsumer) SubscribeJsonSnapshot(name string, config MoqJsonSnapshotConfig) (*MoqJsonSnapshotConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqJsonSnapshotConsumer {
			return FfiConverterMoqJsonSnapshotConsumerINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_subscribe_json_snapshot(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqJsonSnapshotConfigINSTANCE.Lower(config)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Subscribe to a JSON stream track (lossless append-log) by name.
func (_self *MoqBroadcastConsumer) SubscribeJsonStream(name string, config MoqJsonStreamConfig) (*MoqJsonStreamConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqJsonStreamConsumer {
			return FfiConverterMoqJsonStreamConsumerINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqbroadcastconsumer_subscribe_json_stream(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqJsonStreamConfigINSTANCE.Lower(config)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *MoqBroadcastConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqBroadcastConsumer struct{}

var FfiConverterMoqBroadcastConsumerINSTANCE = FfiConverterMoqBroadcastConsumer{}

func (c FfiConverterMoqBroadcastConsumer) Lift(handle C.uint64_t) *MoqBroadcastConsumer {
	result := &MoqBroadcastConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqbroadcastconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqbroadcastconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqBroadcastConsumer).Destroy)
	return result
}

func (c FfiConverterMoqBroadcastConsumer) Read(reader io.Reader) *MoqBroadcastConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqBroadcastConsumer) Lower(value *MoqBroadcastConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqBroadcastConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqBroadcastConsumer) Write(writer io.Writer, value *MoqBroadcastConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqBroadcastConsumer(handle uint64) *MoqBroadcastConsumer {
	return FfiConverterMoqBroadcastConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqBroadcastConsumer(value *MoqBroadcastConsumer) uint64 {
	return uint64(FfiConverterMoqBroadcastConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqBroadcastConsumer struct{}

func (_ FfiDestroyerMoqBroadcastConsumer) Destroy(value *MoqBroadcastConsumer) {
	value.Destroy()
}

type MoqBroadcastDynamicInterface interface {
	// Cancel all current and future `requested_track()` calls.
	Cancel()
	// Wait for the next subscriber-requested track.
	//
	// Returns a [`MoqTrackRequest`]: accept it for raw writes with
	// [`MoqTrackRequest::accept`], publish media onto it with
	// [`MoqBroadcastProducer::publish_media_on_track`], or reject it with
	// [`MoqTrackRequest::abort`]. The requesting subscriber stays pending until then.
	//
	// Returns an error once the broadcast is closed or aborted.
	RequestedTrack() (*MoqTrackRequest, error)
}
type MoqBroadcastDynamic struct {
	ffiObject FfiObject
}

// Cancel all current and future `requested_track()` calls.
func (_self *MoqBroadcastDynamic) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastDynamic")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastdynamic_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Wait for the next subscriber-requested track.
//
// Returns a [`MoqTrackRequest`]: accept it for raw writes with
// [`MoqTrackRequest::accept`], publish media onto it with
// [`MoqBroadcastProducer::publish_media_on_track`], or reject it with
// [`MoqTrackRequest::abort`]. The requesting subscriber stays pending until then.
//
// Returns an error once the broadcast is closed or aborted.
func (_self *MoqBroadcastDynamic) RequestedTrack() (*MoqTrackRequest, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastDynamic")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqTrackRequest {
			return FfiConverterMoqTrackRequestINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqbroadcastdynamic_requested_track(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *MoqBroadcastDynamic) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqBroadcastDynamic struct{}

var FfiConverterMoqBroadcastDynamicINSTANCE = FfiConverterMoqBroadcastDynamic{}

func (c FfiConverterMoqBroadcastDynamic) Lift(handle C.uint64_t) *MoqBroadcastDynamic {
	result := &MoqBroadcastDynamic{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqbroadcastdynamic(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqbroadcastdynamic(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqBroadcastDynamic).Destroy)
	return result
}

func (c FfiConverterMoqBroadcastDynamic) Read(reader io.Reader) *MoqBroadcastDynamic {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqBroadcastDynamic) Lower(value *MoqBroadcastDynamic) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqBroadcastDynamic")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqBroadcastDynamic) Write(writer io.Writer, value *MoqBroadcastDynamic) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqBroadcastDynamic(handle uint64) *MoqBroadcastDynamic {
	return FfiConverterMoqBroadcastDynamicINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqBroadcastDynamic(value *MoqBroadcastDynamic) uint64 {
	return uint64(FfiConverterMoqBroadcastDynamicINSTANCE.Lower(value))
}

type FfiDestroyerMoqBroadcastDynamic struct{}

func (_ FfiDestroyerMoqBroadcastDynamic) Destroy(value *MoqBroadcastDynamic) {
	value.Destroy()
}

type MoqBroadcastProducerInterface interface {
	// Open an audio track on this broadcast. The catalog rendition is
	// registered immediately so subscribers can find the track even
	// before the first frame is written.
	PublishAudio(name string, input MoqAudioEncoderInput, output MoqAudioEncoderOutput) (*MoqAudioProducer, error)
	// Publish a JSON snapshot track (lossy latest-value) by name.
	//
	// Advertise it in the catalog yourself with
	// [`set_catalog_section`](Self::set_catalog_section) if consumers should discover it.
	PublishJsonSnapshot(name string, config MoqJsonSnapshotConfig) (*MoqJsonSnapshotProducer, error)
	// Publish a JSON stream track (lossless append-log) by name.
	PublishJsonStream(name string, config MoqJsonStreamConfig) (*MoqJsonStreamProducer, error)
	// Create a consumer that reads from this broadcast's tracks.
	Consume() (*MoqBroadcastConsumer, error)
	// Create a dynamic producer that yields tracks requested by subscribers.
	//
	// Hold the returned object for as long as missing track requests should be
	// accepted. Dropping it makes future subscriptions to unknown tracks fail.
	Dynamic() (*MoqBroadcastDynamic, error)
	// Finish this publisher, finalizing the catalog stream and cleanly closing the
	// broadcast so subscribers see a normal end rather than `Error::Dropped`.
	Finish() error
	// Create a new media track for this broadcast.
	//
	// The [`MoqInit`] format selects the codec (or container) for the init bytes and frame payloads;
	// its hints seed the catalog. Hints apply to single-codec formats; container formats auto-detect
	// every track.
	PublishMedia(init MoqInit) (*MoqMediaProducer, error)
	// Publish media on a requested track from
	// [`MoqBroadcastDynamic::requested_track`].
	//
	// The importer accepts the request, which is where the track's timescale is set.
	// [`MoqInit`] carries the format, init bytes, and catalog hints. Only
	// single-track formats are supported.
	PublishMediaOnTrack(request *MoqTrackRequest, init MoqInit) (*MoqMediaProducer, error)
	// Create a media track fed by a raw byte stream with unknown frame
	// boundaries (e.g. piped Annex-B H.264 straight from an encoder).
	//
	// Unlike [`Self::publish_media`], the importer infers frame boundaries, so the caller just pushes
	// bytes via [`MoqMediaStreamProducer::write`]. Only self-describing stream formats are supported
	// (avc3, hev1, av01, fmp4, mkv). [`MoqInit`] carries the format, any
	// seed bytes, and catalog hints.
	PublishMediaStream(init MoqInit) (*MoqMediaStreamProducer, error)
	// Create a track for arbitrary byte payloads, no codec or container.
	//
	// Same pattern as moq-boy's `status` and `command` tracks: raw UTF-8/JSON
	// bytes written directly to moq-lite groups with no media framing. `info` sets
	// track properties (priority, latency_max, timescale); omit for defaults.
	PublishTrack(name string, info *MoqTrackInfo) (*MoqTrackProducer, error)
	// Remove a top-level application catalog section by name.
	//
	// Republishes the catalog if the section existed; a no-op otherwise.
	RemoveCatalogSection(name string) error
	// Set whether the broadcast is announced, keeping the rest of its route (hops, cost).
	//
	// The origin advertises the path only while announced; an unannounced
	// broadcast stays reachable by exact path for subscribes and fetches. This is
	// how a publisher goes on and off the air without tearing down the broadcast.
	SetAnnounce(announce bool) error
	// Set (or replace) a top-level application catalog section by name.
	//
	// `json` is any JSON document (object, array, string, ...) serialized as a UTF-8 string.
	// Errors with [`MoqError::Json`] if `json` doesn't parse, or with the reserved-section
	// error if `name` is `video`/`audio` (owned by the media pipeline). The section is
	// republished on the catalog track immediately.
	SetCatalogSection(name string, json string) error
	// Update the broadcast's route: the hop chain, cost, and announce flag it advertises.
	//
	// Use this as conditions shift (e.g. a standby transcoder lowering its cost
	// once it is warm); consumers observe the change via
	// `MoqBroadcastConsumer::route_updates` and sessions forward it downstream.
	SetRoute(route MoqRoute) error
	// Replace the catalog properties shared by every video rendition.
	//
	// Rotation is clockwise and normalized to the nearest quarter turn. An absent field is removed from the next catalog update.
	SetVideoProperties(properties MoqVideoProperties) error
}
type MoqBroadcastProducer struct {
	ffiObject FfiObject
}

// Create a standalone broadcast, not attached to any origin.
//
// Use it to serve a dynamic broadcast request ([`MoqBroadcastRequest::accept`](crate::origin::MoqBroadcastRequest::accept))
// or for local pub/sub via [`consume`](Self::consume). To publish at a path, use
// [`MoqOriginProducer::create_broadcast`](crate::origin::MoqOriginProducer::create_broadcast) instead.
func NewMoqBroadcastProducer() (*MoqBroadcastProducer, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_constructor_moqbroadcastproducer_new(_uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqBroadcastProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqBroadcastProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Open an audio track on this broadcast. The catalog rendition is
// registered immediately so subscribers can find the track even
// before the first frame is written.
func (_self *MoqBroadcastProducer) PublishAudio(name string, input MoqAudioEncoderInput, output MoqAudioEncoderOutput) (*MoqAudioProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_audio(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqAudioEncoderInputINSTANCE.Lower(input), FfiConverterMoqAudioEncoderOutputINSTANCE.Lower(output), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqAudioProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqAudioProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish a JSON snapshot track (lossy latest-value) by name.
//
// Advertise it in the catalog yourself with
// [`set_catalog_section`](Self::set_catalog_section) if consumers should discover it.
func (_self *MoqBroadcastProducer) PublishJsonSnapshot(name string, config MoqJsonSnapshotConfig) (*MoqJsonSnapshotProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_json_snapshot(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqJsonSnapshotConfigINSTANCE.Lower(config), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqJsonSnapshotProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqJsonSnapshotProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish a JSON stream track (lossless append-log) by name.
func (_self *MoqBroadcastProducer) PublishJsonStream(name string, config MoqJsonStreamConfig) (*MoqJsonStreamProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_json_stream(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterMoqJsonStreamConfigINSTANCE.Lower(config), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqJsonStreamProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqJsonStreamProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a consumer that reads from this broadcast's tracks.
func (_self *MoqBroadcastProducer) Consume() (*MoqBroadcastConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_consume(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqBroadcastConsumer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqBroadcastConsumerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a dynamic producer that yields tracks requested by subscribers.
//
// Hold the returned object for as long as missing track requests should be
// accepted. Dropping it makes future subscriptions to unknown tracks fail.
func (_self *MoqBroadcastProducer) Dynamic() (*MoqBroadcastDynamic, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_dynamic(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqBroadcastDynamic
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqBroadcastDynamicINSTANCE.Lift(_uniffiRV), nil
	}
}

// Finish this publisher, finalizing the catalog stream and cleanly closing the
// broadcast so subscribers see a normal end rather than `Error::Dropped`.
func (_self *MoqBroadcastProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Create a new media track for this broadcast.
//
// The [`MoqInit`] format selects the codec (or container) for the init bytes and frame payloads;
// its hints seed the catalog. Hints apply to single-codec formats; container formats auto-detect
// every track.
func (_self *MoqBroadcastProducer) PublishMedia(init MoqInit) (*MoqMediaProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_media(
			_pointer, FfiConverterMoqInitINSTANCE.Lower(init), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqMediaProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqMediaProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Publish media on a requested track from
// [`MoqBroadcastDynamic::requested_track`].
//
// The importer accepts the request, which is where the track's timescale is set.
// [`MoqInit`] carries the format, init bytes, and catalog hints. Only
// single-track formats are supported.
func (_self *MoqBroadcastProducer) PublishMediaOnTrack(request *MoqTrackRequest, init MoqInit) (*MoqMediaProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_media_on_track(
			_pointer, FfiConverterMoqTrackRequestINSTANCE.Lower(request), FfiConverterMoqInitINSTANCE.Lower(init), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqMediaProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqMediaProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a media track fed by a raw byte stream with unknown frame
// boundaries (e.g. piped Annex-B H.264 straight from an encoder).
//
// Unlike [`Self::publish_media`], the importer infers frame boundaries, so the caller just pushes
// bytes via [`MoqMediaStreamProducer::write`]. Only self-describing stream formats are supported
// (avc3, hev1, av01, fmp4, mkv). [`MoqInit`] carries the format, any
// seed bytes, and catalog hints.
func (_self *MoqBroadcastProducer) PublishMediaStream(init MoqInit) (*MoqMediaStreamProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_media_stream(
			_pointer, FfiConverterMoqInitINSTANCE.Lower(init), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqMediaStreamProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqMediaStreamProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a track for arbitrary byte payloads, no codec or container.
//
// Same pattern as moq-boy's `status` and `command` tracks: raw UTF-8/JSON
// bytes written directly to moq-lite groups with no media framing. `info` sets
// track properties (priority, latency_max, timescale); omit for defaults.
func (_self *MoqBroadcastProducer) PublishTrack(name string, info *MoqTrackInfo) (*MoqTrackProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_publish_track(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterOptionalMoqTrackInfoINSTANCE.Lower(info), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Remove a top-level application catalog section by name.
//
// Republishes the catalog if the section existed; a no-op otherwise.
func (_self *MoqBroadcastProducer) RemoveCatalogSection(name string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_remove_catalog_section(
			_pointer, FfiConverterStringINSTANCE.Lower(name), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Set whether the broadcast is announced, keeping the rest of its route (hops, cost).
//
// The origin advertises the path only while announced; an unannounced
// broadcast stays reachable by exact path for subscribes and fetches. This is
// how a publisher goes on and off the air without tearing down the broadcast.
func (_self *MoqBroadcastProducer) SetAnnounce(announce bool) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_set_announce(
			_pointer, FfiConverterBoolINSTANCE.Lower(announce), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Set (or replace) a top-level application catalog section by name.
//
// `json` is any JSON document (object, array, string, ...) serialized as a UTF-8 string.
// Errors with [`MoqError::Json`] if `json` doesn't parse, or with the reserved-section
// error if `name` is `video`/`audio` (owned by the media pipeline). The section is
// republished on the catalog track immediately.
func (_self *MoqBroadcastProducer) SetCatalogSection(name string, json string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_set_catalog_section(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterStringINSTANCE.Lower(json), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Update the broadcast's route: the hop chain, cost, and announce flag it advertises.
//
// Use this as conditions shift (e.g. a standby transcoder lowering its cost
// once it is warm); consumers observe the change via
// `MoqBroadcastConsumer::route_updates` and sessions forward it downstream.
func (_self *MoqBroadcastProducer) SetRoute(route MoqRoute) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_set_route(
			_pointer, FfiConverterMoqRouteINSTANCE.Lower(route), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Replace the catalog properties shared by every video rendition.
//
// Rotation is clockwise and normalized to the nearest quarter turn. An absent field is removed from the next catalog update.
func (_self *MoqBroadcastProducer) SetVideoProperties(properties MoqVideoProperties) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastproducer_set_video_properties(
			_pointer, FfiConverterMoqVideoPropertiesINSTANCE.Lower(properties), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqBroadcastProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqBroadcastProducer struct{}

var FfiConverterMoqBroadcastProducerINSTANCE = FfiConverterMoqBroadcastProducer{}

func (c FfiConverterMoqBroadcastProducer) Lift(handle C.uint64_t) *MoqBroadcastProducer {
	result := &MoqBroadcastProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqbroadcastproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqbroadcastproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqBroadcastProducer).Destroy)
	return result
}

func (c FfiConverterMoqBroadcastProducer) Read(reader io.Reader) *MoqBroadcastProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqBroadcastProducer) Lower(value *MoqBroadcastProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqBroadcastProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqBroadcastProducer) Write(writer io.Writer, value *MoqBroadcastProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqBroadcastProducer(handle uint64) *MoqBroadcastProducer {
	return FfiConverterMoqBroadcastProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqBroadcastProducer(value *MoqBroadcastProducer) uint64 {
	return uint64(FfiConverterMoqBroadcastProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqBroadcastProducer struct{}

func (_ FfiDestroyerMoqBroadcastProducer) Destroy(value *MoqBroadcastProducer) {
	value.Destroy()
}

type MoqBroadcastRequestInterface interface {
	// Abort the request with an application error code.
	Abort(errorCode uint16) error
	// Accept the request with an unannounced broadcast.
	Accept(broadcast *MoqBroadcastProducer) error
	// The requested broadcast path.
	Path() (string, error)
}
type MoqBroadcastRequest struct {
	ffiObject FfiObject
}

// Abort the request with an application error code.
func (_self *MoqBroadcastRequest) Abort(errorCode uint16) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastRequest")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastrequest_abort(
			_pointer, FfiConverterUint16INSTANCE.Lower(errorCode), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Accept the request with an unannounced broadcast.
func (_self *MoqBroadcastRequest) Accept(broadcast *MoqBroadcastProducer) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastRequest")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqbroadcastrequest_accept(
			_pointer, FfiConverterMoqBroadcastProducerINSTANCE.Lower(broadcast), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// The requested broadcast path.
func (_self *MoqBroadcastRequest) Path() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqBroadcastRequest")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqbroadcastrequest_path(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}
func (object *MoqBroadcastRequest) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqBroadcastRequest struct{}

var FfiConverterMoqBroadcastRequestINSTANCE = FfiConverterMoqBroadcastRequest{}

func (c FfiConverterMoqBroadcastRequest) Lift(handle C.uint64_t) *MoqBroadcastRequest {
	result := &MoqBroadcastRequest{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqbroadcastrequest(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqbroadcastrequest(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqBroadcastRequest).Destroy)
	return result
}

func (c FfiConverterMoqBroadcastRequest) Read(reader io.Reader) *MoqBroadcastRequest {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqBroadcastRequest) Lower(value *MoqBroadcastRequest) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqBroadcastRequest")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqBroadcastRequest) Write(writer io.Writer, value *MoqBroadcastRequest) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqBroadcastRequest(handle uint64) *MoqBroadcastRequest {
	return FfiConverterMoqBroadcastRequestINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqBroadcastRequest(value *MoqBroadcastRequest) uint64 {
	return uint64(FfiConverterMoqBroadcastRequestINSTANCE.Lower(value))
}

type FfiDestroyerMoqBroadcastRequest struct{}

func (_ FfiDestroyerMoqBroadcastRequest) Destroy(value *MoqBroadcastRequest) {
	value.Destroy()
}

type MoqCatalogConsumerInterface interface {
	// Cancel all current and future `next()` calls.
	Cancel()
	// Get the next catalog update. Returns `None` when the track ends or is closed.
	Next() (*MoqCatalog, error)
}
type MoqCatalogConsumer struct {
	ffiObject FfiObject
}

// Cancel all current and future `next()` calls.
func (_self *MoqCatalogConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqCatalogConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqcatalogconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Get the next catalog update. Returns `None` when the track ends or is closed.
func (_self *MoqCatalogConsumer) Next() (*MoqCatalog, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqCatalogConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqCatalog {
			return FfiConverterOptionalMoqCatalogINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqcatalogconsumer_next(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *MoqCatalogConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqCatalogConsumer struct{}

var FfiConverterMoqCatalogConsumerINSTANCE = FfiConverterMoqCatalogConsumer{}

func (c FfiConverterMoqCatalogConsumer) Lift(handle C.uint64_t) *MoqCatalogConsumer {
	result := &MoqCatalogConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqcatalogconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqcatalogconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqCatalogConsumer).Destroy)
	return result
}

func (c FfiConverterMoqCatalogConsumer) Read(reader io.Reader) *MoqCatalogConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqCatalogConsumer) Lower(value *MoqCatalogConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqCatalogConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqCatalogConsumer) Write(writer io.Writer, value *MoqCatalogConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqCatalogConsumer(handle uint64) *MoqCatalogConsumer {
	return FfiConverterMoqCatalogConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqCatalogConsumer(value *MoqCatalogConsumer) uint64 {
	return uint64(FfiConverterMoqCatalogConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqCatalogConsumer struct{}

func (_ FfiDestroyerMoqCatalogConsumer) Destroy(value *MoqCatalogConsumer) {
	value.Destroy()
}

type MoqClientInterface interface {
	// Cancel all current and future `connect()` calls.
	Cancel()
	// Connect to a MoQ server and wait for the session to be established.
	//
	// Both origin sides are always accessible via [`MoqSession::publisher`] and
	// [`MoqSession::consumer`], without the caller constructing a [`MoqOriginProducer`]
	// themselves. With neither [`set_publish`](Self::set_publish) nor
	// [`set_consume`](Self::set_consume) wired, the two sides share one origin, so a broadcast
	// announced on this session is also discoverable through it. Wiring either side opts out of
	// that and gives the other side its own fresh origin.
	//
	// Can be cancelled by calling `cancel()`.
	Connect(url string) (*MoqSession, error)
	// Set the local UDP socket bind address. Defaults to `[::]:0`.
	//
	// Returns an error if the address cannot be parsed.
	SetBind(addr string) error
	// Set the origin to consume remote broadcasts from the remote.
	SetConsume(origin **MoqOriginProducer)
	// Set the origin to publish local broadcasts to the remote.
	SetPublish(origin **MoqOriginProducer)
	// Present this PEM certificate chain when the relay requires mTLS.
	//
	// Only certificates are read from the file; any private keys are ignored. Must be
	// paired with `set_tls_key`, otherwise `connect` fails with an incomplete-auth error.
	// Pass `None` to clear a previously set path.
	SetTlsCert(path *string)
	// Disable TLS certificate verification (for development only).
	SetTlsDisableVerify(disable bool)
	// Pin the peer to a certificate with one of these SHA-256 fingerprints, encoded as hex.
	//
	// This is the native equivalent of the browser's WebTransport `serverCertificateHashes`
	// and accepts the same values a server reports (see `MoqServer.cert_fingerprints`). Use it
	// to trust a self-signed certificate without disabling verification. An empty list clears
	// any pinned fingerprints.
	SetTlsFingerprints(fingerprints []string)
	// Present this PEM private key when the relay requires mTLS.
	//
	// Only the private key is read from the file; any certificates are ignored. Must be
	// paired with `set_tls_cert`, otherwise `connect` fails with an incomplete-auth error.
	// Pass `None` to clear a previously set path.
	SetTlsKey(path *string)
	// Trust these PEM root certificate file(s) instead of the system roots.
	//
	// Pass the paths to PEM-encoded CA certificates. An empty list restores the
	// default behavior of using the platform's native root store.
	SetTlsRoots(paths []string)
	// Configure whether to also trust the platform's native root certificates.
	//
	// By default, system roots are trusted only when no custom roots are configured.
	// Set this to `true` to trust system roots in addition to roots from
	// `set_tls_roots`, or `false` to trust only custom roots.
	SetTlsSystemRoots(systemRoots bool)
}
type MoqClient struct {
	ffiObject FfiObject
}

// Create a new MoQ client with default configuration.
func NewMoqClient() *MoqClient {
	return FfiConverterMoqClientINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_constructor_moqclient_new(_uniffiStatus)
	}))
}

// Cancel all current and future `connect()` calls.
func (_self *MoqClient) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Connect to a MoQ server and wait for the session to be established.
//
// Both origin sides are always accessible via [`MoqSession::publisher`] and
// [`MoqSession::consumer`], without the caller constructing a [`MoqOriginProducer`]
// themselves. With neither [`set_publish`](Self::set_publish) nor
// [`set_consume`](Self::set_consume) wired, the two sides share one origin, so a broadcast
// announced on this session is also discoverable through it. Wiring either side opts out of
// that and gives the other side its own fresh origin.
//
// Can be cancelled by calling `cancel()`.
func (_self *MoqClient) Connect(url string) (*MoqSession, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqSession {
			return FfiConverterMoqSessionINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqclient_connect(
			_pointer, FfiConverterStringINSTANCE.Lower(url)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Set the local UDP socket bind address. Defaults to `[::]:0`.
//
// Returns an error if the address cannot be parsed.
func (_self *MoqClient) SetBind(addr string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_bind(
			_pointer, FfiConverterStringINSTANCE.Lower(addr), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Set the origin to consume remote broadcasts from the remote.
func (_self *MoqClient) SetConsume(origin **MoqOriginProducer) {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_consume(
			_pointer, FfiConverterOptionalMoqOriginProducerINSTANCE.Lower(origin), _uniffiStatus)
		return false
	})
}

// Set the origin to publish local broadcasts to the remote.
func (_self *MoqClient) SetPublish(origin **MoqOriginProducer) {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_publish(
			_pointer, FfiConverterOptionalMoqOriginProducerINSTANCE.Lower(origin), _uniffiStatus)
		return false
	})
}

// Present this PEM certificate chain when the relay requires mTLS.
//
// Only certificates are read from the file; any private keys are ignored. Must be
// paired with `set_tls_key`, otherwise `connect` fails with an incomplete-auth error.
// Pass `None` to clear a previously set path.
func (_self *MoqClient) SetTlsCert(path *string) {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_tls_cert(
			_pointer, FfiConverterOptionalStringINSTANCE.Lower(path), _uniffiStatus)
		return false
	})
}

// Disable TLS certificate verification (for development only).
func (_self *MoqClient) SetTlsDisableVerify(disable bool) {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_tls_disable_verify(
			_pointer, FfiConverterBoolINSTANCE.Lower(disable), _uniffiStatus)
		return false
	})
}

// Pin the peer to a certificate with one of these SHA-256 fingerprints, encoded as hex.
//
// This is the native equivalent of the browser's WebTransport `serverCertificateHashes`
// and accepts the same values a server reports (see `MoqServer.cert_fingerprints`). Use it
// to trust a self-signed certificate without disabling verification. An empty list clears
// any pinned fingerprints.
func (_self *MoqClient) SetTlsFingerprints(fingerprints []string) {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_tls_fingerprints(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(fingerprints), _uniffiStatus)
		return false
	})
}

// Present this PEM private key when the relay requires mTLS.
//
// Only the private key is read from the file; any certificates are ignored. Must be
// paired with `set_tls_cert`, otherwise `connect` fails with an incomplete-auth error.
// Pass `None` to clear a previously set path.
func (_self *MoqClient) SetTlsKey(path *string) {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_tls_key(
			_pointer, FfiConverterOptionalStringINSTANCE.Lower(path), _uniffiStatus)
		return false
	})
}

// Trust these PEM root certificate file(s) instead of the system roots.
//
// Pass the paths to PEM-encoded CA certificates. An empty list restores the
// default behavior of using the platform's native root store.
func (_self *MoqClient) SetTlsRoots(paths []string) {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_tls_roots(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(paths), _uniffiStatus)
		return false
	})
}

// Configure whether to also trust the platform's native root certificates.
//
// By default, system roots are trusted only when no custom roots are configured.
// Set this to `true` to trust system roots in addition to roots from
// `set_tls_roots`, or `false` to trust only custom roots.
func (_self *MoqClient) SetTlsSystemRoots(systemRoots bool) {
	_pointer := _self.ffiObject.incrementPointer("*MoqClient")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqclient_set_tls_system_roots(
			_pointer, FfiConverterBoolINSTANCE.Lower(systemRoots), _uniffiStatus)
		return false
	})
}
func (object *MoqClient) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqClient struct{}

var FfiConverterMoqClientINSTANCE = FfiConverterMoqClient{}

func (c FfiConverterMoqClient) Lift(handle C.uint64_t) *MoqClient {
	result := &MoqClient{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqclient(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqclient(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqClient).Destroy)
	return result
}

func (c FfiConverterMoqClient) Read(reader io.Reader) *MoqClient {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqClient) Lower(value *MoqClient) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqClient")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqClient) Write(writer io.Writer, value *MoqClient) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqClient(handle uint64) *MoqClient {
	return FfiConverterMoqClientINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqClient(value *MoqClient) uint64 {
	return uint64(FfiConverterMoqClientINSTANCE.Lower(value))
}

type FfiDestroyerMoqClient struct{}

func (_ FfiDestroyerMoqClient) Destroy(value *MoqClient) {
	value.Destroy()
}

type MoqGroupConsumerInterface interface {
	Cancel()
	// Read the next frame in this group, including its timestamp.
	//
	// Returns `None` when the group ends.
	ReadFrame() (*MoqFrame, error)
	// The sequence number of this group within the track.
	Sequence() uint64
}
type MoqGroupConsumer struct {
	ffiObject FfiObject
}

func (_self *MoqGroupConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqgroupconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Read the next frame in this group, including its timestamp.
//
// Returns `None` when the group ends.
func (_self *MoqGroupConsumer) ReadFrame() (*MoqFrame, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqFrame {
			return FfiConverterOptionalMoqFrameINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqgroupconsumer_read_frame(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// The sequence number of this group within the track.
func (_self *MoqGroupConsumer) Sequence() uint64 {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupConsumer")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqgroupconsumer_sequence(
			_pointer, _uniffiStatus)
	}))
}
func (object *MoqGroupConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqGroupConsumer struct{}

var FfiConverterMoqGroupConsumerINSTANCE = FfiConverterMoqGroupConsumer{}

func (c FfiConverterMoqGroupConsumer) Lift(handle C.uint64_t) *MoqGroupConsumer {
	result := &MoqGroupConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqgroupconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqgroupconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqGroupConsumer).Destroy)
	return result
}

func (c FfiConverterMoqGroupConsumer) Read(reader io.Reader) *MoqGroupConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqGroupConsumer) Lower(value *MoqGroupConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqGroupConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqGroupConsumer) Write(writer io.Writer, value *MoqGroupConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqGroupConsumer(handle uint64) *MoqGroupConsumer {
	return FfiConverterMoqGroupConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqGroupConsumer(value *MoqGroupConsumer) uint64 {
	return uint64(FfiConverterMoqGroupConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqGroupConsumer struct{}

func (_ FfiDestroyerMoqGroupConsumer) Destroy(value *MoqGroupConsumer) {
	value.Destroy()
}

type MoqGroupProducerInterface interface {
	// Abort this group with an application error code.
	Abort(errorCode uint16) error
	// Create a consumer that reads frames from this group.
	Consume() (*MoqGroupConsumer, error)
	// Mark the group as complete. No more frames can be written.
	Finish() error
	// The sequence number of this group within the track.
	Sequence() uint64
	// Write `frame` into this group.
	//
	// Raw tracks default to a microsecond timescale. Custom timescales may round
	// the timestamp during conversion.
	WriteFrame(frame MoqFrame) error
}
type MoqGroupProducer struct {
	ffiObject FfiObject
}

// Abort this group with an application error code.
func (_self *MoqGroupProducer) Abort(errorCode uint16) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqgroupproducer_abort(
			_pointer, FfiConverterUint16INSTANCE.Lower(errorCode), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Create a consumer that reads frames from this group.
func (_self *MoqGroupProducer) Consume() (*MoqGroupConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqgroupproducer_consume(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqGroupConsumer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqGroupConsumerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Mark the group as complete. No more frames can be written.
func (_self *MoqGroupProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqgroupproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// The sequence number of this group within the track.
func (_self *MoqGroupProducer) Sequence() uint64 {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupProducer")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqgroupproducer_sequence(
			_pointer, _uniffiStatus)
	}))
}

// Write `frame` into this group.
//
// Raw tracks default to a microsecond timescale. Custom timescales may round
// the timestamp during conversion.
func (_self *MoqGroupProducer) WriteFrame(frame MoqFrame) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqgroupproducer_write_frame(
			_pointer, FfiConverterMoqFrameINSTANCE.Lower(frame), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqGroupProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqGroupProducer struct{}

var FfiConverterMoqGroupProducerINSTANCE = FfiConverterMoqGroupProducer{}

func (c FfiConverterMoqGroupProducer) Lift(handle C.uint64_t) *MoqGroupProducer {
	result := &MoqGroupProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqgroupproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqgroupproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqGroupProducer).Destroy)
	return result
}

func (c FfiConverterMoqGroupProducer) Read(reader io.Reader) *MoqGroupProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqGroupProducer) Lower(value *MoqGroupProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqGroupProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqGroupProducer) Write(writer io.Writer, value *MoqGroupProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqGroupProducer(handle uint64) *MoqGroupProducer {
	return FfiConverterMoqGroupProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqGroupProducer(value *MoqGroupProducer) uint64 {
	return uint64(FfiConverterMoqGroupProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqGroupProducer struct{}

func (_ FfiDestroyerMoqGroupProducer) Destroy(value *MoqGroupProducer) {
	value.Destroy()
}

// An uncached group requested by a fetch consumer.
type MoqGroupRequestInterface interface {
	// Reject the fetch with an application error code.
	Abort(errorCode uint16) error
	// Accept the request and return a producer for filling the fetched group.
	Accept() (*MoqGroupProducer, error)
	// The consumer's delivery priority for this fetch.
	Priority() uint8
	// The requested group sequence within the track.
	Sequence() uint64
}

// An uncached group requested by a fetch consumer.
type MoqGroupRequest struct {
	ffiObject FfiObject
}

// Reject the fetch with an application error code.
func (_self *MoqGroupRequest) Abort(errorCode uint16) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupRequest")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqgrouprequest_abort(
			_pointer, FfiConverterUint16INSTANCE.Lower(errorCode), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Accept the request and return a producer for filling the fetched group.
func (_self *MoqGroupRequest) Accept() (*MoqGroupProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupRequest")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqgrouprequest_accept(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqGroupProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqGroupProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// The consumer's delivery priority for this fetch.
func (_self *MoqGroupRequest) Priority() uint8 {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint8INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint8_t {
		return C.uniffi_moq_ffi_fn_method_moqgrouprequest_priority(
			_pointer, _uniffiStatus)
	}))
}

// The requested group sequence within the track.
func (_self *MoqGroupRequest) Sequence() uint64 {
	_pointer := _self.ffiObject.incrementPointer("*MoqGroupRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterUint64INSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqgrouprequest_sequence(
			_pointer, _uniffiStatus)
	}))
}
func (object *MoqGroupRequest) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqGroupRequest struct{}

var FfiConverterMoqGroupRequestINSTANCE = FfiConverterMoqGroupRequest{}

func (c FfiConverterMoqGroupRequest) Lift(handle C.uint64_t) *MoqGroupRequest {
	result := &MoqGroupRequest{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqgrouprequest(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqgrouprequest(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqGroupRequest).Destroy)
	return result
}

func (c FfiConverterMoqGroupRequest) Read(reader io.Reader) *MoqGroupRequest {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqGroupRequest) Lower(value *MoqGroupRequest) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqGroupRequest")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqGroupRequest) Write(writer io.Writer, value *MoqGroupRequest) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqGroupRequest(handle uint64) *MoqGroupRequest {
	return FfiConverterMoqGroupRequestINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqGroupRequest(value *MoqGroupRequest) uint64 {
	return uint64(FfiConverterMoqGroupRequestINSTANCE.Lower(value))
}

type FfiDestroyerMoqGroupRequest struct{}

func (_ FfiDestroyerMoqGroupRequest) Destroy(value *MoqGroupRequest) {
	value.Destroy()
}

// Consumes a JSON snapshot track, yielding the latest reconstructed value.
type MoqJsonSnapshotConsumerInterface interface {
	// Cancel all current and future `next()` calls.
	Cancel()
	// Get the next value as a JSON string. Returns `None` once the track ends.
	//
	// A consumer that has fallen behind collapses the backlog and yields only the latest value.
	Next() (*string, error)
}

// Consumes a JSON snapshot track, yielding the latest reconstructed value.
type MoqJsonSnapshotConsumer struct {
	ffiObject FfiObject
}

// Cancel all current and future `next()` calls.
func (_self *MoqJsonSnapshotConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonSnapshotConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqjsonsnapshotconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Get the next value as a JSON string. Returns `None` once the track ends.
//
// A consumer that has fallen behind collapses the backlog and yields only the latest value.
func (_self *MoqJsonSnapshotConsumer) Next() (*string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonSnapshotConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *string {
			return FfiConverterOptionalStringINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqjsonsnapshotconsumer_next(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *MoqJsonSnapshotConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqJsonSnapshotConsumer struct{}

var FfiConverterMoqJsonSnapshotConsumerINSTANCE = FfiConverterMoqJsonSnapshotConsumer{}

func (c FfiConverterMoqJsonSnapshotConsumer) Lift(handle C.uint64_t) *MoqJsonSnapshotConsumer {
	result := &MoqJsonSnapshotConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqjsonsnapshotconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqjsonsnapshotconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqJsonSnapshotConsumer).Destroy)
	return result
}

func (c FfiConverterMoqJsonSnapshotConsumer) Read(reader io.Reader) *MoqJsonSnapshotConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqJsonSnapshotConsumer) Lower(value *MoqJsonSnapshotConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqJsonSnapshotConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqJsonSnapshotConsumer) Write(writer io.Writer, value *MoqJsonSnapshotConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqJsonSnapshotConsumer(handle uint64) *MoqJsonSnapshotConsumer {
	return FfiConverterMoqJsonSnapshotConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqJsonSnapshotConsumer(value *MoqJsonSnapshotConsumer) uint64 {
	return uint64(FfiConverterMoqJsonSnapshotConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqJsonSnapshotConsumer struct{}

func (_ FfiDestroyerMoqJsonSnapshotConsumer) Destroy(value *MoqJsonSnapshotConsumer) {
	value.Destroy()
}

// Publishes a JSON value that consumers see as a single latest state.
type MoqJsonSnapshotProducerInterface interface {
	// Finish the track, closing any open group.
	Finish() error
	// Publish a new value, encoded as a snapshot or delta automatically. `value` is a JSON
	// document. A no-op if unchanged from the previous update.
	Update(value string) error
}

// Publishes a JSON value that consumers see as a single latest state.
type MoqJsonSnapshotProducer struct {
	ffiObject FfiObject
}

// Finish the track, closing any open group.
func (_self *MoqJsonSnapshotProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonSnapshotProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqjsonsnapshotproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Publish a new value, encoded as a snapshot or delta automatically. `value` is a JSON
// document. A no-op if unchanged from the previous update.
func (_self *MoqJsonSnapshotProducer) Update(value string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonSnapshotProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqjsonsnapshotproducer_update(
			_pointer, FfiConverterStringINSTANCE.Lower(value), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqJsonSnapshotProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqJsonSnapshotProducer struct{}

var FfiConverterMoqJsonSnapshotProducerINSTANCE = FfiConverterMoqJsonSnapshotProducer{}

func (c FfiConverterMoqJsonSnapshotProducer) Lift(handle C.uint64_t) *MoqJsonSnapshotProducer {
	result := &MoqJsonSnapshotProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqjsonsnapshotproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqjsonsnapshotproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqJsonSnapshotProducer).Destroy)
	return result
}

func (c FfiConverterMoqJsonSnapshotProducer) Read(reader io.Reader) *MoqJsonSnapshotProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqJsonSnapshotProducer) Lower(value *MoqJsonSnapshotProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqJsonSnapshotProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqJsonSnapshotProducer) Write(writer io.Writer, value *MoqJsonSnapshotProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqJsonSnapshotProducer(handle uint64) *MoqJsonSnapshotProducer {
	return FfiConverterMoqJsonSnapshotProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqJsonSnapshotProducer(value *MoqJsonSnapshotProducer) uint64 {
	return uint64(FfiConverterMoqJsonSnapshotProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqJsonSnapshotProducer struct{}

func (_ FfiDestroyerMoqJsonSnapshotProducer) Destroy(value *MoqJsonSnapshotProducer) {
	value.Destroy()
}

// Consumes an ordered log of JSON records, yielding every record in order.
type MoqJsonStreamConsumerInterface interface {
	// Cancel all current and future `next()` calls.
	Cancel()
	// Get the next record as a JSON string. Returns `None` once the track ends.
	Next() (*string, error)
}

// Consumes an ordered log of JSON records, yielding every record in order.
type MoqJsonStreamConsumer struct {
	ffiObject FfiObject
}

// Cancel all current and future `next()` calls.
func (_self *MoqJsonStreamConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonStreamConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqjsonstreamconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Get the next record as a JSON string. Returns `None` once the track ends.
func (_self *MoqJsonStreamConsumer) Next() (*string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonStreamConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *string {
			return FfiConverterOptionalStringINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqjsonstreamconsumer_next(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *MoqJsonStreamConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqJsonStreamConsumer struct{}

var FfiConverterMoqJsonStreamConsumerINSTANCE = FfiConverterMoqJsonStreamConsumer{}

func (c FfiConverterMoqJsonStreamConsumer) Lift(handle C.uint64_t) *MoqJsonStreamConsumer {
	result := &MoqJsonStreamConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqjsonstreamconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqjsonstreamconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqJsonStreamConsumer).Destroy)
	return result
}

func (c FfiConverterMoqJsonStreamConsumer) Read(reader io.Reader) *MoqJsonStreamConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqJsonStreamConsumer) Lower(value *MoqJsonStreamConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqJsonStreamConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqJsonStreamConsumer) Write(writer io.Writer, value *MoqJsonStreamConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqJsonStreamConsumer(handle uint64) *MoqJsonStreamConsumer {
	return FfiConverterMoqJsonStreamConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqJsonStreamConsumer(value *MoqJsonStreamConsumer) uint64 {
	return uint64(FfiConverterMoqJsonStreamConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqJsonStreamConsumer struct{}

func (_ FfiDestroyerMoqJsonStreamConsumer) Destroy(value *MoqJsonStreamConsumer) {
	value.Destroy()
}

// Publishes an ordered log of JSON records, one record per append.
type MoqJsonStreamProducerInterface interface {
	// Append one record to the log. `value` is a JSON document.
	Append(value string) error
	// Finish the track, closing the group.
	Finish() error
}

// Publishes an ordered log of JSON records, one record per append.
type MoqJsonStreamProducer struct {
	ffiObject FfiObject
}

// Append one record to the log. `value` is a JSON document.
func (_self *MoqJsonStreamProducer) Append(value string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonStreamProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqjsonstreamproducer_append(
			_pointer, FfiConverterStringINSTANCE.Lower(value), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Finish the track, closing the group.
func (_self *MoqJsonStreamProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqJsonStreamProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqjsonstreamproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqJsonStreamProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqJsonStreamProducer struct{}

var FfiConverterMoqJsonStreamProducerINSTANCE = FfiConverterMoqJsonStreamProducer{}

func (c FfiConverterMoqJsonStreamProducer) Lift(handle C.uint64_t) *MoqJsonStreamProducer {
	result := &MoqJsonStreamProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqjsonstreamproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqjsonstreamproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqJsonStreamProducer).Destroy)
	return result
}

func (c FfiConverterMoqJsonStreamProducer) Read(reader io.Reader) *MoqJsonStreamProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqJsonStreamProducer) Lower(value *MoqJsonStreamProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqJsonStreamProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqJsonStreamProducer) Write(writer io.Writer, value *MoqJsonStreamProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqJsonStreamProducer(handle uint64) *MoqJsonStreamProducer {
	return FfiConverterMoqJsonStreamProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqJsonStreamProducer(value *MoqJsonStreamProducer) uint64 {
	return uint64(FfiConverterMoqJsonStreamProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqJsonStreamProducer struct{}

func (_ FfiDestroyerMoqJsonStreamProducer) Destroy(value *MoqJsonStreamProducer) {
	value.Destroy()
}

type MoqMediaConsumerInterface interface {
	// Cancel all current and future `next()` calls.
	Cancel()
	// Get the next frame. Returns `None` when the track ends or is closed.
	Next() (*MoqMediaFrame, error)
}
type MoqMediaConsumer struct {
	ffiObject FfiObject
}

// Cancel all current and future `next()` calls.
func (_self *MoqMediaConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediaconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Get the next frame. Returns `None` when the track ends or is closed.
func (_self *MoqMediaConsumer) Next() (*MoqMediaFrame, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqMediaFrame {
			return FfiConverterOptionalMoqMediaFrameINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqmediaconsumer_next(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *MoqMediaConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqMediaConsumer struct{}

var FfiConverterMoqMediaConsumerINSTANCE = FfiConverterMoqMediaConsumer{}

func (c FfiConverterMoqMediaConsumer) Lift(handle C.uint64_t) *MoqMediaConsumer {
	result := &MoqMediaConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqmediaconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqmediaconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqMediaConsumer).Destroy)
	return result
}

func (c FfiConverterMoqMediaConsumer) Read(reader io.Reader) *MoqMediaConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqMediaConsumer) Lower(value *MoqMediaConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqMediaConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqMediaConsumer) Write(writer io.Writer, value *MoqMediaConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqMediaConsumer(handle uint64) *MoqMediaConsumer {
	return FfiConverterMoqMediaConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqMediaConsumer(value *MoqMediaConsumer) uint64 {
	return uint64(FfiConverterMoqMediaConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqMediaConsumer struct{}

func (_ FfiDestroyerMoqMediaConsumer) Destroy(value *MoqMediaConsumer) {
	value.Destroy()
}

type MoqMediaProducerInterface interface {
	// Finish this media track and finalize encoding.
	Finish() error
	// Return the name of the media track.
	//
	// Errors for a multi-track container source, which has no single track name.
	Name() (string, error)
	// Wait until this media track has no active consumers.
	//
	// Errors for a multi-track container source, which has no single demand.
	Unused() error
	// Wait until this media track has at least one active consumer.
	//
	// Errors for a multi-track container source, which has no single demand.
	Used() error
	// Write `frame` to this media track.
	//
	// The importer derives keyframe status from the bitstream, so a [`MoqFrame`] carries only
	// the payload and its timestamp.
	WriteFrame(frame MoqFrame) error
}
type MoqMediaProducer struct {
	ffiObject FfiObject
}

// Finish this media track and finalize encoding.
func (_self *MoqMediaProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediaproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Return the name of the media track.
//
// Errors for a multi-track container source, which has no single track name.
func (_self *MoqMediaProducer) Name() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqmediaproducer_name(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Wait until this media track has no active consumers.
//
// Errors for a multi-track container source, which has no single demand.
func (_self *MoqMediaProducer) Unused() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_moq_ffi_fn_method_moqmediaproducer_unused(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Wait until this media track has at least one active consumer.
//
// Errors for a multi-track container source, which has no single demand.
func (_self *MoqMediaProducer) Used() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_moq_ffi_fn_method_moqmediaproducer_used(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Write `frame` to this media track.
//
// The importer derives keyframe status from the bitstream, so a [`MoqFrame`] carries only
// the payload and its timestamp.
func (_self *MoqMediaProducer) WriteFrame(frame MoqFrame) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediaproducer_write_frame(
			_pointer, FfiConverterMoqFrameINSTANCE.Lower(frame), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqMediaProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqMediaProducer struct{}

var FfiConverterMoqMediaProducerINSTANCE = FfiConverterMoqMediaProducer{}

func (c FfiConverterMoqMediaProducer) Lift(handle C.uint64_t) *MoqMediaProducer {
	result := &MoqMediaProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqmediaproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqmediaproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqMediaProducer).Destroy)
	return result
}

func (c FfiConverterMoqMediaProducer) Read(reader io.Reader) *MoqMediaProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqMediaProducer) Lower(value *MoqMediaProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqMediaProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqMediaProducer) Write(writer io.Writer, value *MoqMediaProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqMediaProducer(handle uint64) *MoqMediaProducer {
	return FfiConverterMoqMediaProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqMediaProducer(value *MoqMediaProducer) uint64 {
	return uint64(FfiConverterMoqMediaProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqMediaProducer struct{}

func (_ FfiDestroyerMoqMediaProducer) Destroy(value *MoqMediaProducer) {
	value.Destroy()
}

type MoqMediaStreamProducerInterface interface {
	// Finalize the track.
	//
	// The importer emits each access unit when the *next* one's start code
	// arrives, so a trailing access unit with no following delimiter (e.g. the
	// last frame at EOF) is not emitted. This matches moq-cli's stdin path.
	Finish() error
	// Push raw stream bytes (e.g. Annex-B H.264 from an encoder). The importer
	// frames whole access units and keeps any partial trailing frame for the
	// next call, so callers can write arbitrary chunks.
	Write(payload []byte) error
}
type MoqMediaStreamProducer struct {
	ffiObject FfiObject
}

// Finalize the track.
//
// The importer emits each access unit when the *next* one's start code
// arrives, so a trailing access unit with no following delimiter (e.g. the
// last frame at EOF) is not emitted. This matches moq-cli's stdin path.
func (_self *MoqMediaStreamProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaStreamProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediastreamproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Push raw stream bytes (e.g. Annex-B H.264 from an encoder). The importer
// frames whole access units and keeps any partial trailing frame for the
// next call, so callers can write arbitrary chunks.
func (_self *MoqMediaStreamProducer) Write(payload []byte) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqMediaStreamProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqmediastreamproducer_write(
			_pointer, FfiConverterBytesINSTANCE.Lower(payload), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqMediaStreamProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqMediaStreamProducer struct{}

var FfiConverterMoqMediaStreamProducerINSTANCE = FfiConverterMoqMediaStreamProducer{}

func (c FfiConverterMoqMediaStreamProducer) Lift(handle C.uint64_t) *MoqMediaStreamProducer {
	result := &MoqMediaStreamProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqmediastreamproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqmediastreamproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqMediaStreamProducer).Destroy)
	return result
}

func (c FfiConverterMoqMediaStreamProducer) Read(reader io.Reader) *MoqMediaStreamProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqMediaStreamProducer) Lower(value *MoqMediaStreamProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqMediaStreamProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqMediaStreamProducer) Write(writer io.Writer, value *MoqMediaStreamProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqMediaStreamProducer(handle uint64) *MoqMediaStreamProducer {
	return FfiConverterMoqMediaStreamProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqMediaStreamProducer(value *MoqMediaStreamProducer) uint64 {
	return uint64(FfiConverterMoqMediaStreamProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqMediaStreamProducer struct{}

func (_ FfiDestroyerMoqMediaStreamProducer) Destroy(value *MoqMediaStreamProducer) {
	value.Destroy()
}

type MoqOriginConsumerInterface interface {
	// Subscribe to all broadcast announcements under a prefix.
	Announced(prefix string) (*MoqAnnounced, error)
	// Wait for a specific broadcast to be announced by path.
	AnnouncedBroadcast(path string) (*MoqAnnouncedBroadcast, error)
	// Request a broadcast by path, resolving as soon as it can be served.
	//
	// Returns the announced broadcast immediately if one exists; otherwise falls back to a
	// dynamic handler on the origin (if any) and resolves once it serves the broadcast, or
	// errors if nothing can serve it. Unlike `announced_broadcast`, this does *not* wait
	// indefinitely for a future announcement: it resolves or fails based on what is
	// announced now plus any dynamic fallback. Drop the returned future to cancel.
	RequestBroadcast(path string) (*MoqBroadcastConsumer, error)
}
type MoqOriginConsumer struct {
	ffiObject FfiObject
}

// Subscribe to all broadcast announcements under a prefix.
func (_self *MoqOriginConsumer) Announced(prefix string) (*MoqAnnounced, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginConsumer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqoriginconsumer_announced(
			_pointer, FfiConverterStringINSTANCE.Lower(prefix), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqAnnounced
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqAnnouncedINSTANCE.Lift(_uniffiRV), nil
	}
}

// Wait for a specific broadcast to be announced by path.
func (_self *MoqOriginConsumer) AnnouncedBroadcast(path string) (*MoqAnnouncedBroadcast, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginConsumer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqoriginconsumer_announced_broadcast(
			_pointer, FfiConverterStringINSTANCE.Lower(path), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqAnnouncedBroadcast
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqAnnouncedBroadcastINSTANCE.Lift(_uniffiRV), nil
	}
}

// Request a broadcast by path, resolving as soon as it can be served.
//
// Returns the announced broadcast immediately if one exists; otherwise falls back to a
// dynamic handler on the origin (if any) and resolves once it serves the broadcast, or
// errors if nothing can serve it. Unlike `announced_broadcast`, this does *not* wait
// indefinitely for a future announcement: it resolves or fails based on what is
// announced now plus any dynamic fallback. Drop the returned future to cancel.
func (_self *MoqOriginConsumer) RequestBroadcast(path string) (*MoqBroadcastConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqBroadcastConsumer {
			return FfiConverterMoqBroadcastConsumerINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqoriginconsumer_request_broadcast(
			_pointer, FfiConverterStringINSTANCE.Lower(path)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *MoqOriginConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqOriginConsumer struct{}

var FfiConverterMoqOriginConsumerINSTANCE = FfiConverterMoqOriginConsumer{}

func (c FfiConverterMoqOriginConsumer) Lift(handle C.uint64_t) *MoqOriginConsumer {
	result := &MoqOriginConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqoriginconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqoriginconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqOriginConsumer).Destroy)
	return result
}

func (c FfiConverterMoqOriginConsumer) Read(reader io.Reader) *MoqOriginConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqOriginConsumer) Lower(value *MoqOriginConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqOriginConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqOriginConsumer) Write(writer io.Writer, value *MoqOriginConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqOriginConsumer(handle uint64) *MoqOriginConsumer {
	return FfiConverterMoqOriginConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqOriginConsumer(value *MoqOriginConsumer) uint64 {
	return uint64(FfiConverterMoqOriginConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqOriginConsumer struct{}

func (_ FfiDestroyerMoqOriginConsumer) Destroy(value *MoqOriginConsumer) {
	value.Destroy()
}

type MoqOriginDynamicInterface interface {
	// Cancel all current and future `requested_broadcast()` calls.
	Cancel()
	// Wait for the next requested broadcast that is not announced.
	//
	// Returns a [`MoqBroadcastRequest`]: accept it with a broadcast producer or abort
	// it with an application error code. The requesting consumer stays pending until then.
	RequestedBroadcast() (*MoqBroadcastRequest, error)
}
type MoqOriginDynamic struct {
	ffiObject FfiObject
}

// Cancel all current and future `requested_broadcast()` calls.
func (_self *MoqOriginDynamic) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginDynamic")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqorigindynamic_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Wait for the next requested broadcast that is not announced.
//
// Returns a [`MoqBroadcastRequest`]: accept it with a broadcast producer or abort
// it with an application error code. The requesting consumer stays pending until then.
func (_self *MoqOriginDynamic) RequestedBroadcast() (*MoqBroadcastRequest, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginDynamic")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqBroadcastRequest {
			return FfiConverterMoqBroadcastRequestINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqorigindynamic_requested_broadcast(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *MoqOriginDynamic) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqOriginDynamic struct{}

var FfiConverterMoqOriginDynamicINSTANCE = FfiConverterMoqOriginDynamic{}

func (c FfiConverterMoqOriginDynamic) Lift(handle C.uint64_t) *MoqOriginDynamic {
	result := &MoqOriginDynamic{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqorigindynamic(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqorigindynamic(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqOriginDynamic).Destroy)
	return result
}

func (c FfiConverterMoqOriginDynamic) Read(reader io.Reader) *MoqOriginDynamic {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqOriginDynamic) Lower(value *MoqOriginDynamic) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqOriginDynamic")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqOriginDynamic) Write(writer io.Writer, value *MoqOriginDynamic) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqOriginDynamic(handle uint64) *MoqOriginDynamic {
	return FfiConverterMoqOriginDynamicINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqOriginDynamic(value *MoqOriginDynamic) uint64 {
	return uint64(FfiConverterMoqOriginDynamicINSTANCE.Lower(value))
}

type FfiDestroyerMoqOriginDynamic struct{}

func (_ FfiDestroyerMoqOriginDynamic) Destroy(value *MoqOriginDynamic) {
	value.Destroy()
}

type MoqOriginProducerInterface interface {
	// Create a consumer for this origin.
	Consume() *MoqOriginConsumer
	// Create a broadcast at `path` on this origin, returning the producer that feeds it.
	//
	// The broadcast starts announced: the origin advertises the path so subscribers can discover
	// it, becoming visible shortly after this returns. Toggle discoverability with
	// [`MoqBroadcastProducer::set_announce`]; an unannounced broadcast stays reachable by exact
	// path for subscribes and fetches without being announced.
	//
	// [`MoqBroadcastProducer::finish`] unpublishes immediately. Dropping the producer
	// without finishing is treated as a failure: the path lingers briefly so a
	// replacement publisher can take over without subscribers noticing.
	CreateBroadcast(path string) (*MoqBroadcastProducer, error)
	// Create a dynamic handler for serving unannounced broadcasts on request.
	//
	// Hold the returned object while missing broadcast requests should be accepted.
	// Dropping it makes future requests to unknown broadcasts fail.
	Dynamic() *MoqOriginDynamic
}
type MoqOriginProducer struct {
	ffiObject FfiObject
}

// Create a new origin for publishing and/or consuming broadcasts.
func NewMoqOriginProducer(options MoqOriginOptions) *MoqOriginProducer {
	return FfiConverterMoqOriginProducerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_constructor_moqoriginproducer_new(FfiConverterMoqOriginOptionsINSTANCE.Lower(options), _uniffiStatus)
	}))
}

// Create a consumer for this origin.
func (_self *MoqOriginProducer) Consume() *MoqOriginConsumer {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginProducer")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqOriginConsumerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqoriginproducer_consume(
			_pointer, _uniffiStatus)
	}))
}

// Create a broadcast at `path` on this origin, returning the producer that feeds it.
//
// The broadcast starts announced: the origin advertises the path so subscribers can discover
// it, becoming visible shortly after this returns. Toggle discoverability with
// [`MoqBroadcastProducer::set_announce`]; an unannounced broadcast stays reachable by exact
// path for subscribes and fetches without being announced.
//
// [`MoqBroadcastProducer::finish`] unpublishes immediately. Dropping the producer
// without finishing is treated as a failure: the path lingers briefly so a
// replacement publisher can take over without subscribers noticing.
func (_self *MoqOriginProducer) CreateBroadcast(path string) (*MoqBroadcastProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqoriginproducer_create_broadcast(
			_pointer, FfiConverterStringINSTANCE.Lower(path), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqBroadcastProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqBroadcastProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a dynamic handler for serving unannounced broadcasts on request.
//
// Hold the returned object while missing broadcast requests should be accepted.
// Dropping it makes future requests to unknown broadcasts fail.
func (_self *MoqOriginProducer) Dynamic() *MoqOriginDynamic {
	_pointer := _self.ffiObject.incrementPointer("*MoqOriginProducer")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqOriginDynamicINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqoriginproducer_dynamic(
			_pointer, _uniffiStatus)
	}))
}
func (object *MoqOriginProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqOriginProducer struct{}

var FfiConverterMoqOriginProducerINSTANCE = FfiConverterMoqOriginProducer{}

func (c FfiConverterMoqOriginProducer) Lift(handle C.uint64_t) *MoqOriginProducer {
	result := &MoqOriginProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqoriginproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqoriginproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqOriginProducer).Destroy)
	return result
}

func (c FfiConverterMoqOriginProducer) Read(reader io.Reader) *MoqOriginProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqOriginProducer) Lower(value *MoqOriginProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqOriginProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqOriginProducer) Write(writer io.Writer, value *MoqOriginProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqOriginProducer(handle uint64) *MoqOriginProducer {
	return FfiConverterMoqOriginProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqOriginProducer(value *MoqOriginProducer) uint64 {
	return uint64(FfiConverterMoqOriginProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqOriginProducer struct{}

func (_ FfiDestroyerMoqOriginProducer) Destroy(value *MoqOriginProducer) {
	value.Destroy()
}

// An incoming MoQ session that can be accepted or rejected.
type MoqRequestInterface interface {
	// Complete the MoQ handshake and return the established session.
	//
	// Returns `AlreadyResponded` if `accept()` or `reject()` has already been called.
	Accept() (*MoqSession, error)
	// Cancel any in-flight `accept()` or `reject()` call.
	Cancel()
	// Reject the session with the given HTTP status code.
	//
	// Returns `AlreadyResponded` if `accept()` or `reject()` has already been called.
	Reject(code uint16) error
	// Override the consume origin for this session. Falls back to the server's
	// configured consume origin if unset.
	SetConsume(origin **MoqOriginProducer)
	// Override the publish origin for this session. Falls back to the server's
	// configured publish origin if unset.
	SetPublish(origin **MoqOriginProducer)
	// The transport type, e.g. `"quic"`, `"iroh"`, or `"websocket"`.
	Transport() string
	// The URL provided by the client, if any.
	Url() *string
}

// An incoming MoQ session that can be accepted or rejected.
type MoqRequest struct {
	ffiObject FfiObject
}

// Complete the MoQ handshake and return the established session.
//
// Returns `AlreadyResponded` if `accept()` or `reject()` has already been called.
func (_self *MoqRequest) Accept() (*MoqSession, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqSession {
			return FfiConverterMoqSessionINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqrequest_accept(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Cancel any in-flight `accept()` or `reject()` call.
func (_self *MoqRequest) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqrequest_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Reject the session with the given HTTP status code.
//
// Returns `AlreadyResponded` if `accept()` or `reject()` has already been called.
func (_self *MoqRequest) Reject(code uint16) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_moq_ffi_fn_method_moqrequest_reject(
			_pointer, FfiConverterUint16INSTANCE.Lower(code)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Override the consume origin for this session. Falls back to the server's
// configured consume origin if unset.
func (_self *MoqRequest) SetConsume(origin **MoqOriginProducer) {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqrequest_set_consume(
			_pointer, FfiConverterOptionalMoqOriginProducerINSTANCE.Lower(origin), _uniffiStatus)
		return false
	})
}

// Override the publish origin for this session. Falls back to the server's
// configured publish origin if unset.
func (_self *MoqRequest) SetPublish(origin **MoqOriginProducer) {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqrequest_set_publish(
			_pointer, FfiConverterOptionalMoqOriginProducerINSTANCE.Lower(origin), _uniffiStatus)
		return false
	})
}

// The transport type, e.g. `"quic"`, `"iroh"`, or `"websocket"`.
func (_self *MoqRequest) Transport() string {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqrequest_transport(
				_pointer, _uniffiStatus),
		}
	}))
}

// The URL provided by the client, if any.
func (_self *MoqRequest) Url() *string {
	_pointer := _self.ffiObject.incrementPointer("*MoqRequest")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqrequest_url(
				_pointer, _uniffiStatus),
		}
	}))
}
func (object *MoqRequest) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqRequest struct{}

var FfiConverterMoqRequestINSTANCE = FfiConverterMoqRequest{}

func (c FfiConverterMoqRequest) Lift(handle C.uint64_t) *MoqRequest {
	result := &MoqRequest{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqrequest(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqrequest(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqRequest).Destroy)
	return result
}

func (c FfiConverterMoqRequest) Read(reader io.Reader) *MoqRequest {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqRequest) Lower(value *MoqRequest) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqRequest")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqRequest) Write(writer io.Writer, value *MoqRequest) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqRequest(handle uint64) *MoqRequest {
	return FfiConverterMoqRequestINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqRequest(value *MoqRequest) uint64 {
	return uint64(FfiConverterMoqRequestINSTANCE.Lower(value))
}

type FfiDestroyerMoqRequest struct{}

func (_ FfiDestroyerMoqRequest) Destroy(value *MoqRequest) {
	value.Destroy()
}

// A watch over a broadcast's route. Created by `MoqBroadcastConsumer::route_updates`.
type MoqRouteWatchInterface interface {
	// Cancel all current and future `next()` calls.
	Cancel()
	// Wait for the next route: the current one on the first call, then each change.
	//
	// Returns `None` once the broadcast ends (every producer gone).
	Next() (*MoqRoute, error)
}

// A watch over a broadcast's route. Created by `MoqBroadcastConsumer::route_updates`.
type MoqRouteWatch struct {
	ffiObject FfiObject
}

// Cancel all current and future `next()` calls.
func (_self *MoqRouteWatch) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqRouteWatch")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqroutewatch_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Wait for the next route: the current one on the first call, then each change.
//
// Returns `None` once the broadcast ends (every producer gone).
func (_self *MoqRouteWatch) Next() (*MoqRoute, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqRouteWatch")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqRoute {
			return FfiConverterOptionalMoqRouteINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqroutewatch_next(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *MoqRouteWatch) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqRouteWatch struct{}

var FfiConverterMoqRouteWatchINSTANCE = FfiConverterMoqRouteWatch{}

func (c FfiConverterMoqRouteWatch) Lift(handle C.uint64_t) *MoqRouteWatch {
	result := &MoqRouteWatch{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqroutewatch(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqroutewatch(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqRouteWatch).Destroy)
	return result
}

func (c FfiConverterMoqRouteWatch) Read(reader io.Reader) *MoqRouteWatch {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqRouteWatch) Lower(value *MoqRouteWatch) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqRouteWatch")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqRouteWatch) Write(writer io.Writer, value *MoqRouteWatch) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqRouteWatch(handle uint64) *MoqRouteWatch {
	return FfiConverterMoqRouteWatchINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqRouteWatch(value *MoqRouteWatch) uint64 {
	return uint64(FfiConverterMoqRouteWatchINSTANCE.Lower(value))
}

type FfiDestroyerMoqRouteWatch struct{}

func (_ FfiDestroyerMoqRouteWatch) Destroy(value *MoqRouteWatch) {
	value.Destroy()
}

// A MoQ server that accepts incoming QUIC/WebTransport sessions.
type MoqServerInterface interface {
	// Accept the next incoming session. Returns `None` when the server has closed.
	//
	// `listen()` must be called first.
	Accept() (**MoqRequest, error)
	// Cancel any in-flight `listen()` or `accept()` call.
	Cancel()
	// SHA-256 fingerprints of the configured TLS certificates, hex-encoded.
	//
	// Useful for pinning a generated self-signed certificate in a browser via
	// WebTransport's `serverCertificateHashes`. Returns an error if called
	// before `listen()`.
	CertFingerprints() ([]string, error)
	// Bind the listening socket. Returns the bound local address as a string,
	// which is useful when binding to an ephemeral port (`:0`).
	Listen() (string, error)
	// Set the address to bind, e.g. `127.0.0.1:4443`, `[::]:443`, or `localhost:0`.
	//
	// Validated syntactically up-front. DNS hostnames are accepted and resolved
	// at `listen()` time.
	SetBind(addr string) error
	// Set the origin to consume broadcasts from incoming sessions.
	SetConsume(origin **MoqOriginProducer)
	// Set the origin to publish broadcasts to incoming sessions.
	SetPublish(origin **MoqOriginProducer)
	// Load TLS certificate chains from PEM files on disk.
	SetTlsCert(paths []string)
	// Generate self-signed TLS certificates for the given hostnames.
	//
	// Clients must either pin the certificate fingerprint or disable verification.
	SetTlsGenerate(hostnames []string)
	// Load TLS private keys from PEM files on disk.
	SetTlsKey(paths []string)
}

// A MoQ server that accepts incoming QUIC/WebTransport sessions.
type MoqServer struct {
	ffiObject FfiObject
}

// Create a new MoQ server with default configuration.
func NewMoqServer() *MoqServer {
	return FfiConverterMoqServerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_constructor_moqserver_new(_uniffiStatus)
	}))
}

// Accept the next incoming session. Returns `None` when the server has closed.
//
// `listen()` must be called first.
func (_self *MoqServer) Accept() (**MoqRequest, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) **MoqRequest {
			return FfiConverterOptionalMoqRequestINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqserver_accept(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Cancel any in-flight `listen()` or `accept()` call.
func (_self *MoqServer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqserver_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// SHA-256 fingerprints of the configured TLS certificates, hex-encoded.
//
// Useful for pinning a generated self-signed certificate in a browser via
// WebTransport's `serverCertificateHashes`. Returns an error if called
// before `listen()`.
func (_self *MoqServer) CertFingerprints() ([]string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqserver_cert_fingerprints(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue []string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterSequenceStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Bind the listening socket. Returns the bound local address as a string,
// which is useful when binding to an ephemeral port (`:0`).
func (_self *MoqServer) Listen() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) string {
			return FfiConverterStringINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqserver_listen(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Set the address to bind, e.g. `127.0.0.1:4443`, `[::]:443`, or `localhost:0`.
//
// Validated syntactically up-front. DNS hostnames are accepted and resolved
// at `listen()` time.
func (_self *MoqServer) SetBind(addr string) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqserver_set_bind(
			_pointer, FfiConverterStringINSTANCE.Lower(addr), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Set the origin to consume broadcasts from incoming sessions.
func (_self *MoqServer) SetConsume(origin **MoqOriginProducer) {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqserver_set_consume(
			_pointer, FfiConverterOptionalMoqOriginProducerINSTANCE.Lower(origin), _uniffiStatus)
		return false
	})
}

// Set the origin to publish broadcasts to incoming sessions.
func (_self *MoqServer) SetPublish(origin **MoqOriginProducer) {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqserver_set_publish(
			_pointer, FfiConverterOptionalMoqOriginProducerINSTANCE.Lower(origin), _uniffiStatus)
		return false
	})
}

// Load TLS certificate chains from PEM files on disk.
func (_self *MoqServer) SetTlsCert(paths []string) {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqserver_set_tls_cert(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(paths), _uniffiStatus)
		return false
	})
}

// Generate self-signed TLS certificates for the given hostnames.
//
// Clients must either pin the certificate fingerprint or disable verification.
func (_self *MoqServer) SetTlsGenerate(hostnames []string) {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqserver_set_tls_generate(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(hostnames), _uniffiStatus)
		return false
	})
}

// Load TLS private keys from PEM files on disk.
func (_self *MoqServer) SetTlsKey(paths []string) {
	_pointer := _self.ffiObject.incrementPointer("*MoqServer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqserver_set_tls_key(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(paths), _uniffiStatus)
		return false
	})
}
func (object *MoqServer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqServer struct{}

var FfiConverterMoqServerINSTANCE = FfiConverterMoqServer{}

func (c FfiConverterMoqServer) Lift(handle C.uint64_t) *MoqServer {
	result := &MoqServer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqserver(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqserver(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqServer).Destroy)
	return result
}

func (c FfiConverterMoqServer) Read(reader io.Reader) *MoqServer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqServer) Lower(value *MoqServer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqServer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqServer) Write(writer io.Writer, value *MoqServer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqServer(handle uint64) *MoqServer {
	return FfiConverterMoqServerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqServer(value *MoqServer) uint64 {
	return uint64(FfiConverterMoqServerINSTANCE.Lower(value))
}

type FfiDestroyerMoqServer struct{}

func (_ FfiDestroyerMoqServer) Destroy(value *MoqServer) {
	value.Destroy()
}

type MoqSessionInterface interface {
	// Close the session with the given error code.
	Cancel(code uint32)
	// Wait until the session is closed.
	Closed() error
	// The subscribe-side origin: a read handle for receiving
	// announcements pushed by the remote. Either derived from the
	// origin the caller wired via `set_consume`, or auto-created if
	// neither was set.
	Consumer() *MoqOriginConsumer
	// The publish-side origin: where local broadcasts get advertised
	// to the remote. Either the producer the caller wired via
	// `set_publish` / `set_consume` before connect/accept, or one
	// auto-created if neither was set.
	Publisher() *MoqOriginProducer
	// Graceful shutdown. Equivalent to `cancel(0)`. Documents the
	// convention that code 0 means "no error" so callers don't have to
	// pick one. Named `shutdown` (not `close`) because UniFFI's Kotlin
	// generator already emits an `AutoCloseable.close()` that releases
	// the FFI handle, and shadowing it would silently mean a different
	// thing per binding.
	Shutdown()
	// Snapshot the current connection statistics (RTT, bandwidth estimates,
	// byte/packet counters). Cheap to call; intended for periodic polling.
	//
	// Individual fields are `None` when the transport backend doesn't report
	// them; see [`MoqConnectionStats`].
	Stats() MoqConnectionStats
}
type MoqSession struct {
	ffiObject FfiObject
}

// Close the session with the given error code.
func (_self *MoqSession) Cancel(code uint32) {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqsession_cancel(
			_pointer, FfiConverterUint32INSTANCE.Lower(code), _uniffiStatus)
		return false
	})
}

// Wait until the session is closed.
func (_self *MoqSession) Closed() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_moq_ffi_fn_method_moqsession_closed(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// The subscribe-side origin: a read handle for receiving
// announcements pushed by the remote. Either derived from the
// origin the caller wired via `set_consume`, or auto-created if
// neither was set.
func (_self *MoqSession) Consumer() *MoqOriginConsumer {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqOriginConsumerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqsession_consumer(
			_pointer, _uniffiStatus)
	}))
}

// The publish-side origin: where local broadcasts get advertised
// to the remote. Either the producer the caller wired via
// `set_publish` / `set_consume` before connect/accept, or one
// auto-created if neither was set.
func (_self *MoqSession) Publisher() *MoqOriginProducer {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqOriginProducerINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqsession_publisher(
			_pointer, _uniffiStatus)
	}))
}

// Graceful shutdown. Equivalent to `cancel(0)`. Documents the
// convention that code 0 means "no error" so callers don't have to
// pick one. Named `shutdown` (not `close`) because UniFFI's Kotlin
// generator already emits an `AutoCloseable.close()` that releases
// the FFI handle, and shadowing it would silently mean a different
// thing per binding.
func (_self *MoqSession) Shutdown() {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqsession_shutdown(
			_pointer, _uniffiStatus)
		return false
	})
}

// Snapshot the current connection statistics (RTT, bandwidth estimates,
// byte/packet counters). Cheap to call; intended for periodic polling.
//
// Individual fields are `None` when the transport backend doesn't report
// them; see [`MoqConnectionStats`].
func (_self *MoqSession) Stats() MoqConnectionStats {
	_pointer := _self.ffiObject.incrementPointer("*MoqSession")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterMoqConnectionStatsINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqsession_stats(
				_pointer, _uniffiStatus),
		}
	}))
}
func (object *MoqSession) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqSession struct{}

var FfiConverterMoqSessionINSTANCE = FfiConverterMoqSession{}

func (c FfiConverterMoqSession) Lift(handle C.uint64_t) *MoqSession {
	result := &MoqSession{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqsession(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqsession(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqSession).Destroy)
	return result
}

func (c FfiConverterMoqSession) Read(reader io.Reader) *MoqSession {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqSession) Lower(value *MoqSession) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqSession")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqSession) Write(writer io.Writer, value *MoqSession) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqSession(handle uint64) *MoqSession {
	return FfiConverterMoqSessionINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqSession(value *MoqSession) uint64 {
	return uint64(FfiConverterMoqSessionINSTANCE.Lower(value))
}

type FfiDestroyerMoqSession struct{}

func (_ FfiDestroyerMoqSession) Destroy(value *MoqSession) {
	value.Destroy()
}

type MoqTrackConsumerInterface interface {
	Cancel()
	// Return the publisher-side track properties learned during subscription.
	Info() (MoqTrackInfo, error)
	// Return the next group in sequence order, skipping forward if the reader
	// has fallen behind. Returns `None` when the track ends.
	NextGroup() (**MoqGroupConsumer, error)
	// Read the first frame of the next group, including its timestamp.
	//
	// Convenience for tracks using one-frame-per-group (like moq-boy's
	// status/command tracks). Returns `None` when the track ends.
	ReadFrame() (*MoqFrame, error)
	// Receive the next best-effort datagram in arrival order.
	//
	// Returns `None` when the track ends. Datagram delivery is unavailable over
	// IETF moq-transport, pre-lite-05 moq-lite, and stream-only transports.
	RecvDatagram() (*MoqDatagram, error)
	// Return the next group in arrival order. Returns `None` when the track ends.
	//
	// Groups are returned as they arrive on the wire, which may be out of sequence
	// order (e.g. if a later group lands before an earlier one on a separate stream).
	RecvGroup() (**MoqGroupConsumer, error)
	// Change this subscriber's delivery preferences.
	//
	// Silently ignored if the track already ended; the update is meaningless at
	// that point.
	Update(subscription MoqSubscription)
}
type MoqTrackConsumer struct {
	ffiObject FfiObject
}

func (_self *MoqTrackConsumer) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackconsumer_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Return the publisher-side track properties learned during subscription.
func (_self *MoqTrackConsumer) Info() (MoqTrackInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqtrackconsumer_info(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue MoqTrackInfo
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackInfoINSTANCE.Lift(_uniffiRV), nil
	}
}

// Return the next group in sequence order, skipping forward if the reader
// has fallen behind. Returns `None` when the track ends.
func (_self *MoqTrackConsumer) NextGroup() (**MoqGroupConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) **MoqGroupConsumer {
			return FfiConverterOptionalMoqGroupConsumerINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqtrackconsumer_next_group(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Read the first frame of the next group, including its timestamp.
//
// Convenience for tracks using one-frame-per-group (like moq-boy's
// status/command tracks). Returns `None` when the track ends.
func (_self *MoqTrackConsumer) ReadFrame() (*MoqFrame, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqFrame {
			return FfiConverterOptionalMoqFrameINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqtrackconsumer_read_frame(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Receive the next best-effort datagram in arrival order.
//
// Returns `None` when the track ends. Datagram delivery is unavailable over
// IETF moq-transport, pre-lite-05 moq-lite, and stream-only transports.
func (_self *MoqTrackConsumer) RecvDatagram() (*MoqDatagram, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) *MoqDatagram {
			return FfiConverterOptionalMoqDatagramINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqtrackconsumer_recv_datagram(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Return the next group in arrival order. Returns `None` when the track ends.
//
// Groups are returned as they arrive on the wire, which may be out of sequence
// order (e.g. if a later group lands before an earlier one on a separate stream).
func (_self *MoqTrackConsumer) RecvGroup() (**MoqGroupConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_moq_ffi_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) **MoqGroupConsumer {
			return FfiConverterOptionalMoqGroupConsumerINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqtrackconsumer_recv_group(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Change this subscriber's delivery preferences.
//
// Silently ignored if the track already ended; the update is meaningless at
// that point.
func (_self *MoqTrackConsumer) Update(subscription MoqSubscription) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackconsumer_update(
			_pointer, FfiConverterMoqSubscriptionINSTANCE.Lower(subscription), _uniffiStatus)
		return false
	})
}
func (object *MoqTrackConsumer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqTrackConsumer struct{}

var FfiConverterMoqTrackConsumerINSTANCE = FfiConverterMoqTrackConsumer{}

func (c FfiConverterMoqTrackConsumer) Lift(handle C.uint64_t) *MoqTrackConsumer {
	result := &MoqTrackConsumer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqtrackconsumer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqtrackconsumer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqTrackConsumer).Destroy)
	return result
}

func (c FfiConverterMoqTrackConsumer) Read(reader io.Reader) *MoqTrackConsumer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqTrackConsumer) Lower(value *MoqTrackConsumer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqTrackConsumer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqTrackConsumer) Write(writer io.Writer, value *MoqTrackConsumer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqTrackConsumer(handle uint64) *MoqTrackConsumer {
	return FfiConverterMoqTrackConsumerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqTrackConsumer(value *MoqTrackConsumer) uint64 {
	return uint64(FfiConverterMoqTrackConsumerINSTANCE.Lower(value))
}

type FfiDestroyerMoqTrackConsumer struct{}

func (_ FfiDestroyerMoqTrackConsumer) Destroy(value *MoqTrackConsumer) {
	value.Destroy()
}

// Serves on-demand fetches of uncached groups for one track.
type MoqTrackDynamicInterface interface {
	// Cancel all current and future `requested_group()` calls.
	Cancel()
	// Wait for the next fetch of an uncached group.
	//
	// Accept the returned request to produce the group, or abort it with an
	// application error. Cached groups are served without reaching this method.
	RequestedGroup() (*MoqGroupRequest, error)
}

// Serves on-demand fetches of uncached groups for one track.
type MoqTrackDynamic struct {
	ffiObject FfiObject
}

// Cancel all current and future `requested_group()` calls.
func (_self *MoqTrackDynamic) Cancel() {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackDynamic")
	defer _self.ffiObject.decrementPointer()
	rustCall(func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackdynamic_cancel(
			_pointer, _uniffiStatus)
		return false
	})
}

// Wait for the next fetch of an uncached group.
//
// Accept the returned request to produce the group, or abort it with an
// application error. Cached groups are served without reaching this method.
func (_self *MoqTrackDynamic) RequestedGroup() (*MoqGroupRequest, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackDynamic")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_moq_ffi_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *MoqGroupRequest {
			return FfiConverterMoqGroupRequestINSTANCE.Lift(ffi)
		},
		C.uniffi_moq_ffi_fn_method_moqtrackdynamic_requested_group(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *MoqTrackDynamic) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqTrackDynamic struct{}

var FfiConverterMoqTrackDynamicINSTANCE = FfiConverterMoqTrackDynamic{}

func (c FfiConverterMoqTrackDynamic) Lift(handle C.uint64_t) *MoqTrackDynamic {
	result := &MoqTrackDynamic{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqtrackdynamic(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqtrackdynamic(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqTrackDynamic).Destroy)
	return result
}

func (c FfiConverterMoqTrackDynamic) Read(reader io.Reader) *MoqTrackDynamic {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqTrackDynamic) Lower(value *MoqTrackDynamic) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqTrackDynamic")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqTrackDynamic) Write(writer io.Writer, value *MoqTrackDynamic) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqTrackDynamic(handle uint64) *MoqTrackDynamic {
	return FfiConverterMoqTrackDynamicINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqTrackDynamic(value *MoqTrackDynamic) uint64 {
	return uint64(FfiConverterMoqTrackDynamicINSTANCE.Lower(value))
}

type FfiDestroyerMoqTrackDynamic struct{}

func (_ FfiDestroyerMoqTrackDynamic) Destroy(value *MoqTrackDynamic) {
	value.Destroy()
}

type MoqTrackProducerInterface interface {
	// Abort this track with an application error code.
	Abort(errorCode uint16) error
	// Send `frame` as a best-effort datagram, returning the sequence number assigned to it.
	//
	// The payload must be at most 1200 bytes. Datagrams are only delivered on transports and
	// wire versions with a datagram channel; there is no stream fallback.
	AppendDatagram(frame MoqFrame) (uint64, error)
	// Append a new group to the track, returning a producer for writing frames into it.
	AppendGroup() (*MoqGroupProducer, error)
	// Create a consumer that reads from this producer's track.
	//
	// Useful for local pub/sub without going through an origin/broadcast. `subscription`
	// tunes delivery priority, group ordering priority, and group range; omit for defaults.
	Consume(subscription *MoqSubscription) (*MoqTrackConsumer, error)
	// Create a group with an explicit sequence number.
	//
	// Use this for sparse or replayed tracks. [`append_group`](Self::append_group)
	// remains the convenient live-stream path.
	CreateGroup(sequence uint64) (*MoqGroupProducer, error)
	// Create a handler for uncached group fetches on this track.
	//
	// Hold the returned object for as long as cache misses should wait to be
	// served. Without a live dynamic handler, a missing group fails with `NotFound`.
	Dynamic() (*MoqTrackDynamic, error)
	// Release this producer, ending the track at the live edge.
	//
	// [`finish_at`](Self::finish_at) declares the boundary ahead of time, so this keeps
	// that boundary and only releases the producer.
	Finish() error
	// Declare the exclusive final group sequence, possibly ahead of the live edge.
	//
	// Groups below `final_sequence` may still be created afterwards. Groups at or
	// above it are rejected. The producer remains open for groups below the boundary;
	// call [`finish`](Self::finish) after producing the remaining groups.
	FinishAt(finalSequence uint64) error
	// Return the name of this track.
	Name() (string, error)
	// Wait until this track has no active consumers.
	Unused() error
	// Wait until this track has at least one active consumer.
	Used() error
	// Write `frame` as a single-frame group.
	//
	// Raw tracks default to a microsecond timescale. Custom timescales may round
	// the timestamp during conversion.
	WriteFrame(frame MoqFrame) error
}
type MoqTrackProducer struct {
	ffiObject FfiObject
}

// Abort this track with an application error code.
func (_self *MoqTrackProducer) Abort(errorCode uint16) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackproducer_abort(
			_pointer, FfiConverterUint16INSTANCE.Lower(errorCode), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Send `frame` as a best-effort datagram, returning the sequence number assigned to it.
//
// The payload must be at most 1200 bytes. Datagrams are only delivered on transports and
// wire versions with a datagram channel; there is no stream fallback.
func (_self *MoqTrackProducer) AppendDatagram(frame MoqFrame) (uint64, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackproducer_append_datagram(
			_pointer, FfiConverterMoqFrameINSTANCE.Lower(frame), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue uint64
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterUint64INSTANCE.Lift(_uniffiRV), nil
	}
}

// Append a new group to the track, returning a producer for writing frames into it.
func (_self *MoqTrackProducer) AppendGroup() (*MoqGroupProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackproducer_append_group(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqGroupProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqGroupProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a consumer that reads from this producer's track.
//
// Useful for local pub/sub without going through an origin/broadcast. `subscription`
// tunes delivery priority, group ordering priority, and group range; omit for defaults.
func (_self *MoqTrackProducer) Consume(subscription *MoqSubscription) (*MoqTrackConsumer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackproducer_consume(
			_pointer, FfiConverterOptionalMoqSubscriptionINSTANCE.Lower(subscription), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackConsumer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackConsumerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a group with an explicit sequence number.
//
// Use this for sparse or replayed tracks. [`append_group`](Self::append_group)
// remains the convenient live-stream path.
func (_self *MoqTrackProducer) CreateGroup(sequence uint64) (*MoqGroupProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackproducer_create_group(
			_pointer, FfiConverterUint64INSTANCE.Lower(sequence), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqGroupProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqGroupProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a handler for uncached group fetches on this track.
//
// Hold the returned object for as long as cache misses should wait to be
// served. Without a live dynamic handler, a missing group fails with `NotFound`.
func (_self *MoqTrackProducer) Dynamic() (*MoqTrackDynamic, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackproducer_dynamic(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackDynamic
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackDynamicINSTANCE.Lift(_uniffiRV), nil
	}
}

// Release this producer, ending the track at the live edge.
//
// [`finish_at`](Self::finish_at) declares the boundary ahead of time, so this keeps
// that boundary and only releases the producer.
func (_self *MoqTrackProducer) Finish() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackproducer_finish(
			_pointer, _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Declare the exclusive final group sequence, possibly ahead of the live edge.
//
// Groups below `final_sequence` may still be created afterwards. Groups at or
// above it are rejected. The producer remains open for groups below the boundary;
// call [`finish`](Self::finish) after producing the remaining groups.
func (_self *MoqTrackProducer) FinishAt(finalSequence uint64) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackproducer_finish_at(
			_pointer, FfiConverterUint64INSTANCE.Lower(finalSequence), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Return the name of this track.
func (_self *MoqTrackProducer) Name() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqtrackproducer_name(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}

// Wait until this track has no active consumers.
func (_self *MoqTrackProducer) Unused() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_moq_ffi_fn_method_moqtrackproducer_unused(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Wait until this track has at least one active consumer.
func (_self *MoqTrackProducer) Used() error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*MoqError](
		FfiConverterMoqErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_moq_ffi_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_moq_ffi_fn_method_moqtrackproducer_used(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_moq_ffi_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_moq_ffi_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// Write `frame` as a single-frame group.
//
// Raw tracks default to a microsecond timescale. Custom timescales may round
// the timestamp during conversion.
func (_self *MoqTrackProducer) WriteFrame(frame MoqFrame) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackProducer")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackproducer_write_frame(
			_pointer, FfiConverterMoqFrameINSTANCE.Lower(frame), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
func (object *MoqTrackProducer) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqTrackProducer struct{}

var FfiConverterMoqTrackProducerINSTANCE = FfiConverterMoqTrackProducer{}

func (c FfiConverterMoqTrackProducer) Lift(handle C.uint64_t) *MoqTrackProducer {
	result := &MoqTrackProducer{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqtrackproducer(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqtrackproducer(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqTrackProducer).Destroy)
	return result
}

func (c FfiConverterMoqTrackProducer) Read(reader io.Reader) *MoqTrackProducer {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqTrackProducer) Lower(value *MoqTrackProducer) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqTrackProducer")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqTrackProducer) Write(writer io.Writer, value *MoqTrackProducer) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqTrackProducer(handle uint64) *MoqTrackProducer {
	return FfiConverterMoqTrackProducerINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqTrackProducer(value *MoqTrackProducer) uint64 {
	return uint64(FfiConverterMoqTrackProducerINSTANCE.Lower(value))
}

type FfiDestroyerMoqTrackProducer struct{}

func (_ FfiDestroyerMoqTrackProducer) Destroy(value *MoqTrackProducer) {
	value.Destroy()
}

// A track requested by a subscriber that hasn't been accepted yet.
//
// Mirrors [`moq_net::track::Request`]: [`accept`](Self::accept) it to start producing raw
// frames, hand it to [`MoqBroadcastProducer::publish_media_on_track`] to publish media,
// or [`abort`](Self::abort) it to reject the waiting subscriber.
type MoqTrackRequestInterface interface {
	// Reject the request with an application error code, failing the waiting subscriber.
	Abort(errorCode uint16) error
	// Accept the request as a raw track, fixing its [`MoqTrackInfo`] (timescale, etc.).
	//
	// For media use [`MoqBroadcastProducer::publish_media_on_track`] instead, which lets
	// the importer pick the timescale.
	Accept(info *MoqTrackInfo) (*MoqTrackProducer, error)
	// Create a handler for uncached group fetches before accepting this track.
	//
	// Obtain and retain this handle before `accept()` when the track itself was
	// requested by a fetch. This keeps the pending group request serviceable across
	// the transition from request to producer.
	Dynamic() (*MoqTrackDynamic, error)
	// The requested track name.
	Name() (string, error)
}

// A track requested by a subscriber that hasn't been accepted yet.
//
// Mirrors [`moq_net::track::Request`]: [`accept`](Self::accept) it to start producing raw
// frames, hand it to [`MoqBroadcastProducer::publish_media_on_track`] to publish media,
// or [`abort`](Self::abort) it to reject the waiting subscriber.
type MoqTrackRequest struct {
	ffiObject FfiObject
}

// Reject the request with an application error code, failing the waiting subscriber.
func (_self *MoqTrackRequest) Abort(errorCode uint16) error {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackRequest")
	defer _self.ffiObject.decrementPointer()
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_method_moqtrackrequest_abort(
			_pointer, FfiConverterUint16INSTANCE.Lower(errorCode), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}

// Accept the request as a raw track, fixing its [`MoqTrackInfo`] (timescale, etc.).
//
// For media use [`MoqBroadcastProducer::publish_media_on_track`] instead, which lets
// the importer pick the timescale.
func (_self *MoqTrackRequest) Accept(info *MoqTrackInfo) (*MoqTrackProducer, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackRequest")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackrequest_accept(
			_pointer, FfiConverterOptionalMoqTrackInfoINSTANCE.Lower(info), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackProducer
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackProducerINSTANCE.Lift(_uniffiRV), nil
	}
}

// Create a handler for uncached group fetches before accepting this track.
//
// Obtain and retain this handle before `accept()` when the track itself was
// requested by a fetch. This keeps the pending group request serviceable across
// the transition from request to producer.
func (_self *MoqTrackRequest) Dynamic() (*MoqTrackDynamic, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackRequest")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_moq_ffi_fn_method_moqtrackrequest_dynamic(
			_pointer, _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *MoqTrackDynamic
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterMoqTrackDynamicINSTANCE.Lift(_uniffiRV), nil
	}
}

// The requested track name.
func (_self *MoqTrackRequest) Name() (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*MoqTrackRequest")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_moq_ffi_fn_method_moqtrackrequest_name(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue string
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterStringINSTANCE.Lift(_uniffiRV), nil
	}
}
func (object *MoqTrackRequest) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterMoqTrackRequest struct{}

var FfiConverterMoqTrackRequestINSTANCE = FfiConverterMoqTrackRequest{}

func (c FfiConverterMoqTrackRequest) Lift(handle C.uint64_t) *MoqTrackRequest {
	result := &MoqTrackRequest{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_moq_ffi_fn_clone_moqtrackrequest(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_moq_ffi_fn_free_moqtrackrequest(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*MoqTrackRequest).Destroy)
	return result
}

func (c FfiConverterMoqTrackRequest) Read(reader io.Reader) *MoqTrackRequest {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterMoqTrackRequest) Lower(value *MoqTrackRequest) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*MoqTrackRequest")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterMoqTrackRequest) Write(writer io.Writer, value *MoqTrackRequest) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalMoqTrackRequest(handle uint64) *MoqTrackRequest {
	return FfiConverterMoqTrackRequestINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalMoqTrackRequest(value *MoqTrackRequest) uint64 {
	return uint64(FfiConverterMoqTrackRequestINSTANCE.Lower(value))
}

type FfiDestroyerMoqTrackRequest struct{}

func (_ FfiDestroyerMoqTrackRequest) Destroy(value *MoqTrackRequest) {
	value.Destroy()
}

type MoqAudio struct {
	Codec        string
	Description  *[]byte
	SampleRate   uint32
	ChannelCount uint32
	Bitrate      *uint64
	Container    MoqContainer
}

func (r *MoqAudio) Destroy() {
	FfiDestroyerString{}.Destroy(r.Codec)
	FfiDestroyerOptionalBytes{}.Destroy(r.Description)
	FfiDestroyerUint32{}.Destroy(r.SampleRate)
	FfiDestroyerUint32{}.Destroy(r.ChannelCount)
	FfiDestroyerOptionalUint64{}.Destroy(r.Bitrate)
	FfiDestroyerMoqContainer{}.Destroy(r.Container)
}

type FfiConverterMoqAudio struct{}

var FfiConverterMoqAudioINSTANCE = FfiConverterMoqAudio{}

func (c FfiConverterMoqAudio) Lift(rb RustBufferI) MoqAudio {
	return LiftFromRustBuffer[MoqAudio](c, rb)
}

func (c FfiConverterMoqAudio) Read(reader io.Reader) MoqAudio {
	return MoqAudio{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterMoqContainerINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqAudio) Lower(value MoqAudio) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudio](c, value)
}

func (c FfiConverterMoqAudio) LowerExternal(value MoqAudio) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudio](c, value))
}

func (c FfiConverterMoqAudio) Write(writer io.Writer, value MoqAudio) {
	FfiConverterStringINSTANCE.Write(writer, value.Codec)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.Description)
	FfiConverterUint32INSTANCE.Write(writer, value.SampleRate)
	FfiConverterUint32INSTANCE.Write(writer, value.ChannelCount)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Bitrate)
	FfiConverterMoqContainerINSTANCE.Write(writer, value.Container)
}

type FfiDestroyerMoqAudio struct{}

func (_ FfiDestroyerMoqAudio) Destroy(value MoqAudio) {
	value.Destroy()
}

// PCM layout the caller wants out of [`MoqAudioConsumer::next`].
type MoqAudioDecoderOutput struct {
	Format MoqAudioFormat
	// `None` delivers samples at the codec's native rate.
	SampleRate *uint32
	// `None` delivers samples at the codec's native channel count.
	Channels *uint32
	// Upper bound on buffering before skipping a stalled group, in
	// milliseconds. Same congestion-control knob as
	// [`MoqSubscription::latency_max_ms`](crate::consumer::MoqSubscription::latency_max_ms):
	// when a group stalls and a newer group is more than this far ahead,
	// the consumer skips. `None` keeps the moq-mux default of zero (skip
	// aggressively). Named `_max` to leave room for a future
	// `latency_min_ms` (jitter buffer).
	LatencyMaxMs *uint64
}

func (r *MoqAudioDecoderOutput) Destroy() {
	FfiDestroyerMoqAudioFormat{}.Destroy(r.Format)
	FfiDestroyerOptionalUint32{}.Destroy(r.SampleRate)
	FfiDestroyerOptionalUint32{}.Destroy(r.Channels)
	FfiDestroyerOptionalUint64{}.Destroy(r.LatencyMaxMs)
}

type FfiConverterMoqAudioDecoderOutput struct{}

var FfiConverterMoqAudioDecoderOutputINSTANCE = FfiConverterMoqAudioDecoderOutput{}

func (c FfiConverterMoqAudioDecoderOutput) Lift(rb RustBufferI) MoqAudioDecoderOutput {
	return LiftFromRustBuffer[MoqAudioDecoderOutput](c, rb)
}

func (c FfiConverterMoqAudioDecoderOutput) Read(reader io.Reader) MoqAudioDecoderOutput {
	return MoqAudioDecoderOutput{
		FfiConverterMoqAudioFormatINSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqAudioDecoderOutput) Lower(value MoqAudioDecoderOutput) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudioDecoderOutput](c, value)
}

func (c FfiConverterMoqAudioDecoderOutput) LowerExternal(value MoqAudioDecoderOutput) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudioDecoderOutput](c, value))
}

func (c FfiConverterMoqAudioDecoderOutput) Write(writer io.Writer, value MoqAudioDecoderOutput) {
	FfiConverterMoqAudioFormatINSTANCE.Write(writer, value.Format)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.SampleRate)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.Channels)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.LatencyMaxMs)
}

type FfiDestroyerMoqAudioDecoderOutput struct{}

func (_ FfiDestroyerMoqAudioDecoderOutput) Destroy(value MoqAudioDecoderOutput) {
	value.Destroy()
}

// PCM layout the caller will pass to [`MoqAudioProducer::write`].
type MoqAudioEncoderInput struct {
	Format     MoqAudioFormat
	SampleRate uint32
	Channels   uint32
}

func (r *MoqAudioEncoderInput) Destroy() {
	FfiDestroyerMoqAudioFormat{}.Destroy(r.Format)
	FfiDestroyerUint32{}.Destroy(r.SampleRate)
	FfiDestroyerUint32{}.Destroy(r.Channels)
}

type FfiConverterMoqAudioEncoderInput struct{}

var FfiConverterMoqAudioEncoderInputINSTANCE = FfiConverterMoqAudioEncoderInput{}

func (c FfiConverterMoqAudioEncoderInput) Lift(rb RustBufferI) MoqAudioEncoderInput {
	return LiftFromRustBuffer[MoqAudioEncoderInput](c, rb)
}

func (c FfiConverterMoqAudioEncoderInput) Read(reader io.Reader) MoqAudioEncoderInput {
	return MoqAudioEncoderInput{
		FfiConverterMoqAudioFormatINSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqAudioEncoderInput) Lower(value MoqAudioEncoderInput) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudioEncoderInput](c, value)
}

func (c FfiConverterMoqAudioEncoderInput) LowerExternal(value MoqAudioEncoderInput) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudioEncoderInput](c, value))
}

func (c FfiConverterMoqAudioEncoderInput) Write(writer io.Writer, value MoqAudioEncoderInput) {
	FfiConverterMoqAudioFormatINSTANCE.Write(writer, value.Format)
	FfiConverterUint32INSTANCE.Write(writer, value.SampleRate)
	FfiConverterUint32INSTANCE.Write(writer, value.Channels)
}

type FfiDestroyerMoqAudioEncoderInput struct{}

func (_ FfiDestroyerMoqAudioEncoderInput) Destroy(value MoqAudioEncoderInput) {
	value.Destroy()
}

// Codec-side configuration. `sample_rate` / `channels` `None` means
// "match the input (snapping the rate up to a libopus-supported
// value if necessary)".
type MoqAudioEncoderOutput struct {
	Codec      MoqAudioCodec
	SampleRate *uint32
	Channels   *uint32
	Bitrate    *uint32
	// Encoded frame duration in milliseconds. Opus accepts
	// 2.5/5/10/20/40/60 ms; pass 20 to match the JS publish path.
	FrameDurationMs uint32
}

func (r *MoqAudioEncoderOutput) Destroy() {
	FfiDestroyerMoqAudioCodec{}.Destroy(r.Codec)
	FfiDestroyerOptionalUint32{}.Destroy(r.SampleRate)
	FfiDestroyerOptionalUint32{}.Destroy(r.Channels)
	FfiDestroyerOptionalUint32{}.Destroy(r.Bitrate)
	FfiDestroyerUint32{}.Destroy(r.FrameDurationMs)
}

type FfiConverterMoqAudioEncoderOutput struct{}

var FfiConverterMoqAudioEncoderOutputINSTANCE = FfiConverterMoqAudioEncoderOutput{}

func (c FfiConverterMoqAudioEncoderOutput) Lift(rb RustBufferI) MoqAudioEncoderOutput {
	return LiftFromRustBuffer[MoqAudioEncoderOutput](c, rb)
}

func (c FfiConverterMoqAudioEncoderOutput) Read(reader io.Reader) MoqAudioEncoderOutput {
	return MoqAudioEncoderOutput{
		FfiConverterMoqAudioCodecINSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqAudioEncoderOutput) Lower(value MoqAudioEncoderOutput) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudioEncoderOutput](c, value)
}

func (c FfiConverterMoqAudioEncoderOutput) LowerExternal(value MoqAudioEncoderOutput) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudioEncoderOutput](c, value))
}

func (c FfiConverterMoqAudioEncoderOutput) Write(writer io.Writer, value MoqAudioEncoderOutput) {
	FfiConverterMoqAudioCodecINSTANCE.Write(writer, value.Codec)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.SampleRate)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.Channels)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.Bitrate)
	FfiConverterUint32INSTANCE.Write(writer, value.FrameDurationMs)
}

type FfiDestroyerMoqAudioEncoderOutput struct{}

func (_ FfiDestroyerMoqAudioEncoderOutput) Destroy(value MoqAudioEncoderOutput) {
	value.Destroy()
}

// One audio frame: payload bytes plus a presentation timestamp.
//
// PCM layout is fixed by the producer / consumer config, so it is
// **not** carried per-frame. On the producer side `data` is raw PCM
// in the configured `input_format`; on the consumer side it is raw
// PCM in the configured `output_format`.
type MoqAudioFrame struct {
	// Presentation timestamp of the first sample, in microseconds.
	TimestampUs uint64
	// The samples, in the configured PCM layout.
	Data []byte
}

func (r *MoqAudioFrame) Destroy() {
	FfiDestroyerUint64{}.Destroy(r.TimestampUs)
	FfiDestroyerBytes{}.Destroy(r.Data)
}

type FfiConverterMoqAudioFrame struct{}

var FfiConverterMoqAudioFrameINSTANCE = FfiConverterMoqAudioFrame{}

func (c FfiConverterMoqAudioFrame) Lift(rb RustBufferI) MoqAudioFrame {
	return LiftFromRustBuffer[MoqAudioFrame](c, rb)
}

func (c FfiConverterMoqAudioFrame) Read(reader io.Reader) MoqAudioFrame {
	return MoqAudioFrame{
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterBytesINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqAudioFrame) Lower(value MoqAudioFrame) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudioFrame](c, value)
}

func (c FfiConverterMoqAudioFrame) LowerExternal(value MoqAudioFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudioFrame](c, value))
}

func (c FfiConverterMoqAudioFrame) Write(writer io.Writer, value MoqAudioFrame) {
	FfiConverterUint64INSTANCE.Write(writer, value.TimestampUs)
	FfiConverterBytesINSTANCE.Write(writer, value.Data)
}

type FfiDestroyerMoqAudioFrame struct{}

func (_ FfiDestroyerMoqAudioFrame) Destroy(value MoqAudioFrame) {
	value.Destroy()
}

type MoqCatalog struct {
	Video    map[string]MoqVideo
	Audio    map[string]MoqAudio
	Display  *MoqDimensions
	Rotation *float64
	Flip     *bool
	// Untyped application catalog sections, keyed by section name, each value a JSON string.
	// These are the top-level catalog keys beyond `video`/`audio`, carried through verbatim
	// (parse the JSON yourself). Set them on the publish side with
	// [`set_catalog_section`](crate::producer::MoqBroadcastProducer::set_catalog_section).
	Sections map[string]string
}

func (r *MoqCatalog) Destroy() {
	FfiDestroyerMapStringMoqVideo{}.Destroy(r.Video)
	FfiDestroyerMapStringMoqAudio{}.Destroy(r.Audio)
	FfiDestroyerOptionalMoqDimensions{}.Destroy(r.Display)
	FfiDestroyerOptionalFloat64{}.Destroy(r.Rotation)
	FfiDestroyerOptionalBool{}.Destroy(r.Flip)
	FfiDestroyerMapStringString{}.Destroy(r.Sections)
}

type FfiConverterMoqCatalog struct{}

var FfiConverterMoqCatalogINSTANCE = FfiConverterMoqCatalog{}

func (c FfiConverterMoqCatalog) Lift(rb RustBufferI) MoqCatalog {
	return LiftFromRustBuffer[MoqCatalog](c, rb)
}

func (c FfiConverterMoqCatalog) Read(reader io.Reader) MoqCatalog {
	return MoqCatalog{
		FfiConverterMapStringMoqVideoINSTANCE.Read(reader),
		FfiConverterMapStringMoqAudioINSTANCE.Read(reader),
		FfiConverterOptionalMoqDimensionsINSTANCE.Read(reader),
		FfiConverterOptionalFloat64INSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
		FfiConverterMapStringStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqCatalog) Lower(value MoqCatalog) C.RustBuffer {
	return LowerIntoRustBuffer[MoqCatalog](c, value)
}

func (c FfiConverterMoqCatalog) LowerExternal(value MoqCatalog) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqCatalog](c, value))
}

func (c FfiConverterMoqCatalog) Write(writer io.Writer, value MoqCatalog) {
	FfiConverterMapStringMoqVideoINSTANCE.Write(writer, value.Video)
	FfiConverterMapStringMoqAudioINSTANCE.Write(writer, value.Audio)
	FfiConverterOptionalMoqDimensionsINSTANCE.Write(writer, value.Display)
	FfiConverterOptionalFloat64INSTANCE.Write(writer, value.Rotation)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.Flip)
	FfiConverterMapStringStringINSTANCE.Write(writer, value.Sections)
}

type FfiDestroyerMoqCatalog struct{}

func (_ FfiDestroyerMoqCatalog) Destroy(value MoqCatalog) {
	value.Destroy()
}

// A snapshot of connection statistics for a [`MoqSession`].
//
// Each field is `None` when the transport backend doesn't report that metric (native QUIC
// reports all of them; the browser WebTransport reports few or none), or when it isn't yet
// available (e.g. `send_rate_bps` before the congestion controller has a window). A `None` is
// not the same as a zero value.
type MoqConnectionStats struct {
	// Smoothed round-trip time, in microseconds.
	RttUs *uint64
	// Estimated send bandwidth from the congestion controller, in bits per second.
	SendRateBps *uint64
	// Estimated receive bandwidth from MoQ PROBE, in bits per second.
	RecvRateBps *uint64
	// Total bytes sent, including retransmissions and overhead.
	BytesSent *uint64
	// Total bytes received, including duplicates and overhead.
	BytesReceived *uint64
	// Total bytes lost (detected via retransmission or acknowledgement).
	BytesLost *uint64
	// Total datagrams sent.
	PacketsSent *uint64
	// Total datagrams received.
	PacketsReceived *uint64
	// Total datagrams detected as lost.
	PacketsLost *uint64
}

func (r *MoqConnectionStats) Destroy() {
	FfiDestroyerOptionalUint64{}.Destroy(r.RttUs)
	FfiDestroyerOptionalUint64{}.Destroy(r.SendRateBps)
	FfiDestroyerOptionalUint64{}.Destroy(r.RecvRateBps)
	FfiDestroyerOptionalUint64{}.Destroy(r.BytesSent)
	FfiDestroyerOptionalUint64{}.Destroy(r.BytesReceived)
	FfiDestroyerOptionalUint64{}.Destroy(r.BytesLost)
	FfiDestroyerOptionalUint64{}.Destroy(r.PacketsSent)
	FfiDestroyerOptionalUint64{}.Destroy(r.PacketsReceived)
	FfiDestroyerOptionalUint64{}.Destroy(r.PacketsLost)
}

type FfiConverterMoqConnectionStats struct{}

var FfiConverterMoqConnectionStatsINSTANCE = FfiConverterMoqConnectionStats{}

func (c FfiConverterMoqConnectionStats) Lift(rb RustBufferI) MoqConnectionStats {
	return LiftFromRustBuffer[MoqConnectionStats](c, rb)
}

func (c FfiConverterMoqConnectionStats) Read(reader io.Reader) MoqConnectionStats {
	return MoqConnectionStats{
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqConnectionStats) Lower(value MoqConnectionStats) C.RustBuffer {
	return LowerIntoRustBuffer[MoqConnectionStats](c, value)
}

func (c FfiConverterMoqConnectionStats) LowerExternal(value MoqConnectionStats) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqConnectionStats](c, value))
}

func (c FfiConverterMoqConnectionStats) Write(writer io.Writer, value MoqConnectionStats) {
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.RttUs)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.SendRateBps)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.RecvRateBps)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.BytesSent)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.BytesReceived)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.BytesLost)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.PacketsSent)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.PacketsReceived)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.PacketsLost)
}

type FfiDestroyerMoqConnectionStats struct{}

func (_ FfiDestroyerMoqConnectionStats) Destroy(value MoqConnectionStats) {
	value.Destroy()
}

// A best-effort raw track datagram, as received.
//
// Send one with [`append_datagram`](crate::producer::MoqTrackProducer::append_datagram), which
// takes a [`MoqFrame`] and assigns the sequence number for you.
type MoqDatagram struct {
	// Per-track sequence number, shared with groups.
	Sequence uint64
	// Presentation timestamp in microseconds.
	TimestampUs uint64
	// Datagram payload, capped at 1200 bytes.
	Payload []byte
}

func (r *MoqDatagram) Destroy() {
	FfiDestroyerUint64{}.Destroy(r.Sequence)
	FfiDestroyerUint64{}.Destroy(r.TimestampUs)
	FfiDestroyerBytes{}.Destroy(r.Payload)
}

type FfiConverterMoqDatagram struct{}

var FfiConverterMoqDatagramINSTANCE = FfiConverterMoqDatagram{}

func (c FfiConverterMoqDatagram) Lift(rb RustBufferI) MoqDatagram {
	return LiftFromRustBuffer[MoqDatagram](c, rb)
}

func (c FfiConverterMoqDatagram) Read(reader io.Reader) MoqDatagram {
	return MoqDatagram{
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterBytesINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqDatagram) Lower(value MoqDatagram) C.RustBuffer {
	return LowerIntoRustBuffer[MoqDatagram](c, value)
}

func (c FfiConverterMoqDatagram) LowerExternal(value MoqDatagram) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqDatagram](c, value))
}

func (c FfiConverterMoqDatagram) Write(writer io.Writer, value MoqDatagram) {
	FfiConverterUint64INSTANCE.Write(writer, value.Sequence)
	FfiConverterUint64INSTANCE.Write(writer, value.TimestampUs)
	FfiConverterBytesINSTANCE.Write(writer, value.Payload)
}

type FfiDestroyerMoqDatagram struct{}

func (_ FfiDestroyerMoqDatagram) Destroy(value MoqDatagram) {
	value.Destroy()
}

type MoqDimensions struct {
	Width  uint32
	Height uint32
}

func (r *MoqDimensions) Destroy() {
	FfiDestroyerUint32{}.Destroy(r.Width)
	FfiDestroyerUint32{}.Destroy(r.Height)
}

type FfiConverterMoqDimensions struct{}

var FfiConverterMoqDimensionsINSTANCE = FfiConverterMoqDimensions{}

func (c FfiConverterMoqDimensions) Lift(rb RustBufferI) MoqDimensions {
	return LiftFromRustBuffer[MoqDimensions](c, rb)
}

func (c FfiConverterMoqDimensions) Read(reader io.Reader) MoqDimensions {
	return MoqDimensions{
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqDimensions) Lower(value MoqDimensions) C.RustBuffer {
	return LowerIntoRustBuffer[MoqDimensions](c, value)
}

func (c FfiConverterMoqDimensions) LowerExternal(value MoqDimensions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqDimensions](c, value))
}

func (c FfiConverterMoqDimensions) Write(writer io.Writer, value MoqDimensions) {
	FfiConverterUint32INSTANCE.Write(writer, value.Width)
	FfiConverterUint32INSTANCE.Write(writer, value.Height)
}

type FfiDestroyerMoqDimensions struct{}

func (_ FfiDestroyerMoqDimensions) Destroy(value MoqDimensions) {
	value.Destroy()
}

// Options for fetching one past group by sequence.
type MoqFetchGroupOptions struct {
	// Delivery priority for the fetch stream; higher values preempt lower ones.
	Priority uint8
}

func (r *MoqFetchGroupOptions) Destroy() {
	FfiDestroyerUint8{}.Destroy(r.Priority)
}

type FfiConverterMoqFetchGroupOptions struct{}

var FfiConverterMoqFetchGroupOptionsINSTANCE = FfiConverterMoqFetchGroupOptions{}

func (c FfiConverterMoqFetchGroupOptions) Lift(rb RustBufferI) MoqFetchGroupOptions {
	return LiftFromRustBuffer[MoqFetchGroupOptions](c, rb)
}

func (c FfiConverterMoqFetchGroupOptions) Read(reader io.Reader) MoqFetchGroupOptions {
	return MoqFetchGroupOptions{
		FfiConverterUint8INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqFetchGroupOptions) Lower(value MoqFetchGroupOptions) C.RustBuffer {
	return LowerIntoRustBuffer[MoqFetchGroupOptions](c, value)
}

func (c FfiConverterMoqFetchGroupOptions) LowerExternal(value MoqFetchGroupOptions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqFetchGroupOptions](c, value))
}

func (c FfiConverterMoqFetchGroupOptions) Write(writer io.Writer, value MoqFetchGroupOptions) {
	FfiConverterUint8INSTANCE.Write(writer, value.Priority)
}

type FfiDestroyerMoqFetchGroupOptions struct{}

func (_ FfiDestroyerMoqFetchGroupOptions) Destroy(value MoqFetchGroupOptions) {
	value.Destroy()
}

// A payload and the time it should be presented.
//
// The unit of both writing and raw reading: every producer write takes one of these, and a
// raw (non-media) read returns one. Media reads return a [`MoqMediaFrame`] instead, which
// adds the codec-derived keyframe flag.
type MoqFrame struct {
	// The frame payload.
	Payload []byte
	// Presentation timestamp in microseconds.
	TimestampUs uint64
}

func (r *MoqFrame) Destroy() {
	FfiDestroyerBytes{}.Destroy(r.Payload)
	FfiDestroyerUint64{}.Destroy(r.TimestampUs)
}

type FfiConverterMoqFrame struct{}

var FfiConverterMoqFrameINSTANCE = FfiConverterMoqFrame{}

func (c FfiConverterMoqFrame) Lift(rb RustBufferI) MoqFrame {
	return LiftFromRustBuffer[MoqFrame](c, rb)
}

func (c FfiConverterMoqFrame) Read(reader io.Reader) MoqFrame {
	return MoqFrame{
		FfiConverterBytesINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqFrame) Lower(value MoqFrame) C.RustBuffer {
	return LowerIntoRustBuffer[MoqFrame](c, value)
}

func (c FfiConverterMoqFrame) LowerExternal(value MoqFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqFrame](c, value))
}

func (c FfiConverterMoqFrame) Write(writer io.Writer, value MoqFrame) {
	FfiConverterBytesINSTANCE.Write(writer, value.Payload)
	FfiConverterUint64INSTANCE.Write(writer, value.TimestampUs)
}

type FfiDestroyerMoqFrame struct{}

func (_ FfiDestroyerMoqFrame) Destroy(value MoqFrame) {
	value.Destroy()
}

// What a single-track media publish needs: a format, its init bytes, and optional video fields.
//
// `format` selects the codec (e.g. `"opus"`, `"avc3"`); `data` carries the codec init bytes (an
// OpusHead, an avcC, an AudioSpecificConfig, ...). Audio formats need those bytes up front; video
// formats may resolve in band, and a [`video`](Self::video) hint pins catalog fields the stream
// can't reveal (bitrate) or publishes the catalog before the first keyframe. See
// [`MoqBroadcastProducer::publish_media`](crate::producer::MoqBroadcastProducer::publish_media).
type MoqInit struct {
	// The media format, e.g. `"opus"`, `"avc3"`, or `"aac"`.
	Format string
	// Codec init bytes. Required for audio; may be empty for a video format that resolves in band.
	Data []byte
	// Caller-provided fields for a video track.
	Video *MoqVideoHint
}

func (r *MoqInit) Destroy() {
	FfiDestroyerString{}.Destroy(r.Format)
	FfiDestroyerBytes{}.Destroy(r.Data)
	FfiDestroyerOptionalMoqVideoHint{}.Destroy(r.Video)
}

type FfiConverterMoqInit struct{}

var FfiConverterMoqInitINSTANCE = FfiConverterMoqInit{}

func (c FfiConverterMoqInit) Lift(rb RustBufferI) MoqInit {
	return LiftFromRustBuffer[MoqInit](c, rb)
}

func (c FfiConverterMoqInit) Read(reader io.Reader) MoqInit {
	return MoqInit{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterBytesINSTANCE.Read(reader),
		FfiConverterOptionalMoqVideoHintINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqInit) Lower(value MoqInit) C.RustBuffer {
	return LowerIntoRustBuffer[MoqInit](c, value)
}

func (c FfiConverterMoqInit) LowerExternal(value MoqInit) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqInit](c, value))
}

func (c FfiConverterMoqInit) Write(writer io.Writer, value MoqInit) {
	FfiConverterStringINSTANCE.Write(writer, value.Format)
	FfiConverterBytesINSTANCE.Write(writer, value.Data)
	FfiConverterOptionalMoqVideoHintINSTANCE.Write(writer, value.Video)
}

type FfiDestroyerMoqInit struct{}

func (_ FfiDestroyerMoqInit) Destroy(value MoqInit) {
	value.Destroy()
}

// Options for a JSON snapshot track (lossy latest-value mode).
//
// The same config is passed to both the producer and the consumer, but the consumer reads only
// [`compression`](Self::compression); [`delta_ratio`](Self::delta_ratio) is producer-only.
type MoqJsonSnapshotConfig struct {
	// How aggressively the producer emits deltas instead of full snapshots. `0` disables deltas
	// (one snapshot per group); a positive value allows roughly that many snapshots' worth of
	// deltas before rolling a new group. Ignored by the consumer.
	DeltaRatio uint32
	// DEFLATE-compress each group. Must match on the producer and consumer.
	Compression bool
}

func (r *MoqJsonSnapshotConfig) Destroy() {
	FfiDestroyerUint32{}.Destroy(r.DeltaRatio)
	FfiDestroyerBool{}.Destroy(r.Compression)
}

type FfiConverterMoqJsonSnapshotConfig struct{}

var FfiConverterMoqJsonSnapshotConfigINSTANCE = FfiConverterMoqJsonSnapshotConfig{}

func (c FfiConverterMoqJsonSnapshotConfig) Lift(rb RustBufferI) MoqJsonSnapshotConfig {
	return LiftFromRustBuffer[MoqJsonSnapshotConfig](c, rb)
}

func (c FfiConverterMoqJsonSnapshotConfig) Read(reader io.Reader) MoqJsonSnapshotConfig {
	return MoqJsonSnapshotConfig{
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqJsonSnapshotConfig) Lower(value MoqJsonSnapshotConfig) C.RustBuffer {
	return LowerIntoRustBuffer[MoqJsonSnapshotConfig](c, value)
}

func (c FfiConverterMoqJsonSnapshotConfig) LowerExternal(value MoqJsonSnapshotConfig) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqJsonSnapshotConfig](c, value))
}

func (c FfiConverterMoqJsonSnapshotConfig) Write(writer io.Writer, value MoqJsonSnapshotConfig) {
	FfiConverterUint32INSTANCE.Write(writer, value.DeltaRatio)
	FfiConverterBoolINSTANCE.Write(writer, value.Compression)
}

type FfiDestroyerMoqJsonSnapshotConfig struct{}

func (_ FfiDestroyerMoqJsonSnapshotConfig) Destroy(value MoqJsonSnapshotConfig) {
	value.Destroy()
}

// Options for a JSON stream track (lossless append-log mode).
//
// The same config is passed to both the producer and the consumer.
type MoqJsonStreamConfig struct {
	// DEFLATE-compress the group. Must match on the producer and consumer.
	Compression bool
}

func (r *MoqJsonStreamConfig) Destroy() {
	FfiDestroyerBool{}.Destroy(r.Compression)
}

type FfiConverterMoqJsonStreamConfig struct{}

var FfiConverterMoqJsonStreamConfigINSTANCE = FfiConverterMoqJsonStreamConfig{}

func (c FfiConverterMoqJsonStreamConfig) Lift(rb RustBufferI) MoqJsonStreamConfig {
	return LiftFromRustBuffer[MoqJsonStreamConfig](c, rb)
}

func (c FfiConverterMoqJsonStreamConfig) Read(reader io.Reader) MoqJsonStreamConfig {
	return MoqJsonStreamConfig{
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqJsonStreamConfig) Lower(value MoqJsonStreamConfig) C.RustBuffer {
	return LowerIntoRustBuffer[MoqJsonStreamConfig](c, value)
}

func (c FfiConverterMoqJsonStreamConfig) LowerExternal(value MoqJsonStreamConfig) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqJsonStreamConfig](c, value))
}

func (c FfiConverterMoqJsonStreamConfig) Write(writer io.Writer, value MoqJsonStreamConfig) {
	FfiConverterBoolINSTANCE.Write(writer, value.Compression)
}

type FfiDestroyerMoqJsonStreamConfig struct{}

func (_ FfiDestroyerMoqJsonStreamConfig) Destroy(value MoqJsonStreamConfig) {
	value.Destroy()
}

// A [`MoqFrame`] plus the codec metadata a media track carries.
type MoqMediaFrame struct {
	// The frame payload.
	Payload []byte
	// Presentation timestamp in microseconds.
	TimestampUs uint64
	// Whether this frame can be decoded without any earlier frame.
	Keyframe bool
}

func (r *MoqMediaFrame) Destroy() {
	FfiDestroyerBytes{}.Destroy(r.Payload)
	FfiDestroyerUint64{}.Destroy(r.TimestampUs)
	FfiDestroyerBool{}.Destroy(r.Keyframe)
}

type FfiConverterMoqMediaFrame struct{}

var FfiConverterMoqMediaFrameINSTANCE = FfiConverterMoqMediaFrame{}

func (c FfiConverterMoqMediaFrame) Lift(rb RustBufferI) MoqMediaFrame {
	return LiftFromRustBuffer[MoqMediaFrame](c, rb)
}

func (c FfiConverterMoqMediaFrame) Read(reader io.Reader) MoqMediaFrame {
	return MoqMediaFrame{
		FfiConverterBytesINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqMediaFrame) Lower(value MoqMediaFrame) C.RustBuffer {
	return LowerIntoRustBuffer[MoqMediaFrame](c, value)
}

func (c FfiConverterMoqMediaFrame) LowerExternal(value MoqMediaFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqMediaFrame](c, value))
}

func (c FfiConverterMoqMediaFrame) Write(writer io.Writer, value MoqMediaFrame) {
	FfiConverterBytesINSTANCE.Write(writer, value.Payload)
	FfiConverterUint64INSTANCE.Write(writer, value.TimestampUs)
	FfiConverterBoolINSTANCE.Write(writer, value.Keyframe)
}

type FfiDestroyerMoqMediaFrame struct{}

func (_ FfiDestroyerMoqMediaFrame) Destroy(value MoqMediaFrame) {
	value.Destroy()
}

// Options used when creating an origin.
type MoqOriginOptions struct {
	// Maximum cached group bytes across broadcasts under this origin. Null is unbounded.
	CacheCapacityBytes *uint64
}

func (r *MoqOriginOptions) Destroy() {
	FfiDestroyerOptionalUint64{}.Destroy(r.CacheCapacityBytes)
}

type FfiConverterMoqOriginOptions struct{}

var FfiConverterMoqOriginOptionsINSTANCE = FfiConverterMoqOriginOptions{}

func (c FfiConverterMoqOriginOptions) Lift(rb RustBufferI) MoqOriginOptions {
	return LiftFromRustBuffer[MoqOriginOptions](c, rb)
}

func (c FfiConverterMoqOriginOptions) Read(reader io.Reader) MoqOriginOptions {
	return MoqOriginOptions{
		FfiConverterOptionalUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqOriginOptions) Lower(value MoqOriginOptions) C.RustBuffer {
	return LowerIntoRustBuffer[MoqOriginOptions](c, value)
}

func (c FfiConverterMoqOriginOptions) LowerExternal(value MoqOriginOptions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqOriginOptions](c, value))
}

func (c FfiConverterMoqOriginOptions) Write(writer io.Writer, value MoqOriginOptions) {
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.CacheCapacityBytes)
}

type FfiDestroyerMoqOriginOptions struct{}

func (_ FfiDestroyerMoqOriginOptions) Destroy(value MoqOriginOptions) {
	value.Destroy()
}

// The path a broadcast takes to reach this origin, and how preferable it is.
//
// Dynamic: it changes when the serving route fails over or the publisher
// re-advertises itself. Publish changes with `MoqBroadcastProducer::set_route`
// and observe them with `MoqBroadcastConsumer::route_updates`.
type MoqRoute struct {
	// Origin ids of the relay hops the broadcast traversed, oldest first.
	Hops []uint64
	// Preference among routes serving the same broadcast: lower wins.
	Cost uint64
	// Whether the broadcast is announced: advertised to subscribers via the origin.
	// An unannounced broadcast stays reachable by exact path for subscribes and fetches.
	Announce bool
}

func (r *MoqRoute) Destroy() {
	FfiDestroyerSequenceUint64{}.Destroy(r.Hops)
	FfiDestroyerUint64{}.Destroy(r.Cost)
	FfiDestroyerBool{}.Destroy(r.Announce)
}

type FfiConverterMoqRoute struct{}

var FfiConverterMoqRouteINSTANCE = FfiConverterMoqRoute{}

func (c FfiConverterMoqRoute) Lift(rb RustBufferI) MoqRoute {
	return LiftFromRustBuffer[MoqRoute](c, rb)
}

func (c FfiConverterMoqRoute) Read(reader io.Reader) MoqRoute {
	return MoqRoute{
		FfiConverterSequenceUint64INSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqRoute) Lower(value MoqRoute) C.RustBuffer {
	return LowerIntoRustBuffer[MoqRoute](c, value)
}

func (c FfiConverterMoqRoute) LowerExternal(value MoqRoute) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqRoute](c, value))
}

func (c FfiConverterMoqRoute) Write(writer io.Writer, value MoqRoute) {
	FfiConverterSequenceUint64INSTANCE.Write(writer, value.Hops)
	FfiConverterUint64INSTANCE.Write(writer, value.Cost)
	FfiConverterBoolINSTANCE.Write(writer, value.Announce)
}

type FfiDestroyerMoqRoute struct{}

func (_ FfiDestroyerMoqRoute) Destroy(value MoqRoute) {
	value.Destroy()
}

// Subscriber-side delivery preferences, mirroring [`moq_net::track::Subscription`].
//
// Construct with the fields you care about; the rest default to moq-net's defaults
// (priority 0, unordered, no staleness tolerance, full group range).
type MoqSubscription struct {
	// Delivery priority; higher values preempt lower ones under bandwidth contention.
	Priority uint8
	// Whether groups are prioritized in sequence order. Groups may always arrive
	// out-of-order (or not at all) over the network. Defaults to `false`; the
	// aggregate is ordered only when every subscriber asks for it.
	Ordered bool
	// Maximum age of a non-latest group before it is skipped, in milliseconds.
	// `0` skips immediately; a larger value tolerates that much reordering.
	//
	// Enforced both by the publisher's cache (sent on the wire) and by any local
	// buffering, such as `subscribe_media`'s jitter buffer.
	LatencyMaxMs uint64
	// First group to deliver, or null to start at the latest group.
	GroupStart *uint64
	// Last group to deliver (inclusive), or null for no end.
	GroupEnd *uint64
}

func (r *MoqSubscription) Destroy() {
	FfiDestroyerUint8{}.Destroy(r.Priority)
	FfiDestroyerBool{}.Destroy(r.Ordered)
	FfiDestroyerUint64{}.Destroy(r.LatencyMaxMs)
	FfiDestroyerOptionalUint64{}.Destroy(r.GroupStart)
	FfiDestroyerOptionalUint64{}.Destroy(r.GroupEnd)
}

type FfiConverterMoqSubscription struct{}

var FfiConverterMoqSubscriptionINSTANCE = FfiConverterMoqSubscription{}

func (c FfiConverterMoqSubscription) Lift(rb RustBufferI) MoqSubscription {
	return LiftFromRustBuffer[MoqSubscription](c, rb)
}

func (c FfiConverterMoqSubscription) Read(reader io.Reader) MoqSubscription {
	return MoqSubscription{
		FfiConverterUint8INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqSubscription) Lower(value MoqSubscription) C.RustBuffer {
	return LowerIntoRustBuffer[MoqSubscription](c, value)
}

func (c FfiConverterMoqSubscription) LowerExternal(value MoqSubscription) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqSubscription](c, value))
}

func (c FfiConverterMoqSubscription) Write(writer io.Writer, value MoqSubscription) {
	FfiConverterUint8INSTANCE.Write(writer, value.Priority)
	FfiConverterBoolINSTANCE.Write(writer, value.Ordered)
	FfiConverterUint64INSTANCE.Write(writer, value.LatencyMaxMs)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.GroupStart)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.GroupEnd)
}

type FfiDestroyerMoqSubscription struct{}

func (_ FfiDestroyerMoqSubscription) Destroy(value MoqSubscription) {
	value.Destroy()
}

// Publisher-side track properties, mirroring [`moq_net::track::Info`].
//
// Construct with the fields you care about; the rest use raw-track defaults
// (priority 0, unordered, default latency budget, microsecond timescale).
type MoqTrackInfo struct {
	// Priority, used only to break ties between subscriptions of equal subscriber priority.
	Priority uint8
	// Whether groups are prioritized in sequence order. Groups may always arrive
	// out-of-order (or not at all) over the network. Defaults to false.
	Ordered bool
	// Maximum age of a non-latest group before the publisher evicts it, in
	// milliseconds. Null uses the default. This is the publisher-side half of
	// [`MoqSubscription::latency_max_ms`](crate::consumer::MoqSubscription::latency_max_ms).
	LatencyMaxMs *uint64
	// Per-frame timescale in ticks per second. Null uses microseconds.
	Timescale *uint64
}

func (r *MoqTrackInfo) Destroy() {
	FfiDestroyerUint8{}.Destroy(r.Priority)
	FfiDestroyerBool{}.Destroy(r.Ordered)
	FfiDestroyerOptionalUint64{}.Destroy(r.LatencyMaxMs)
	FfiDestroyerOptionalUint64{}.Destroy(r.Timescale)
}

type FfiConverterMoqTrackInfo struct{}

var FfiConverterMoqTrackInfoINSTANCE = FfiConverterMoqTrackInfo{}

func (c FfiConverterMoqTrackInfo) Lift(rb RustBufferI) MoqTrackInfo {
	return LiftFromRustBuffer[MoqTrackInfo](c, rb)
}

func (c FfiConverterMoqTrackInfo) Read(reader io.Reader) MoqTrackInfo {
	return MoqTrackInfo{
		FfiConverterUint8INSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqTrackInfo) Lower(value MoqTrackInfo) C.RustBuffer {
	return LowerIntoRustBuffer[MoqTrackInfo](c, value)
}

func (c FfiConverterMoqTrackInfo) LowerExternal(value MoqTrackInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqTrackInfo](c, value))
}

func (c FfiConverterMoqTrackInfo) Write(writer io.Writer, value MoqTrackInfo) {
	FfiConverterUint8INSTANCE.Write(writer, value.Priority)
	FfiConverterBoolINSTANCE.Write(writer, value.Ordered)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.LatencyMaxMs)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Timescale)
}

type FfiDestroyerMoqTrackInfo struct{}

func (_ FfiDestroyerMoqTrackInfo) Destroy(value MoqTrackInfo) {
	value.Destroy()
}

type MoqVideo struct {
	Codec         string
	Description   *[]byte
	Coded         *MoqDimensions
	DisplayAspect *MoqDimensions
	Bitrate       *uint64
	Framerate     *float64
	Container     MoqContainer
}

func (r *MoqVideo) Destroy() {
	FfiDestroyerString{}.Destroy(r.Codec)
	FfiDestroyerOptionalBytes{}.Destroy(r.Description)
	FfiDestroyerOptionalMoqDimensions{}.Destroy(r.Coded)
	FfiDestroyerOptionalMoqDimensions{}.Destroy(r.DisplayAspect)
	FfiDestroyerOptionalUint64{}.Destroy(r.Bitrate)
	FfiDestroyerOptionalFloat64{}.Destroy(r.Framerate)
	FfiDestroyerMoqContainer{}.Destroy(r.Container)
}

type FfiConverterMoqVideo struct{}

var FfiConverterMoqVideoINSTANCE = FfiConverterMoqVideo{}

func (c FfiConverterMoqVideo) Lift(rb RustBufferI) MoqVideo {
	return LiftFromRustBuffer[MoqVideo](c, rb)
}

func (c FfiConverterMoqVideo) Read(reader io.Reader) MoqVideo {
	return MoqVideo{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
		FfiConverterOptionalMoqDimensionsINSTANCE.Read(reader),
		FfiConverterOptionalMoqDimensionsINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalFloat64INSTANCE.Read(reader),
		FfiConverterMoqContainerINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqVideo) Lower(value MoqVideo) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideo](c, value)
}

func (c FfiConverterMoqVideo) LowerExternal(value MoqVideo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideo](c, value))
}

func (c FfiConverterMoqVideo) Write(writer io.Writer, value MoqVideo) {
	FfiConverterStringINSTANCE.Write(writer, value.Codec)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.Description)
	FfiConverterOptionalMoqDimensionsINSTANCE.Write(writer, value.Coded)
	FfiConverterOptionalMoqDimensionsINSTANCE.Write(writer, value.DisplayAspect)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Bitrate)
	FfiConverterOptionalFloat64INSTANCE.Write(writer, value.Framerate)
	FfiConverterMoqContainerINSTANCE.Write(writer, value.Container)
}

type FfiDestroyerMoqVideo struct{}

func (_ FfiDestroyerMoqVideo) Destroy(value MoqVideo) {
	value.Destroy()
}

// Caller-provided video catalog fields for [`MoqInit`].
//
// Every field is optional and fills only a gap the stream leaves; a value the stream detects wins.
// Publishing the catalog before the first keyframe needs at least the codec, which comes from the
// [`MoqInit`] format. Audio has no equivalent: an audio format resolves entirely from its init bytes.
type MoqVideoHint struct {
	// The encoded pixel dimensions.
	Coded *MoqDimensions
	// The display aspect ratio.
	DisplayAspect *MoqDimensions
	// The maximum bitrate in bits per second.
	Bitrate *uint64
	// The frame rate in frames per second.
	Framerate *float64
	// Whether the decoder should optimize for latency.
	OptimizeForLatency *bool
}

func (r *MoqVideoHint) Destroy() {
	FfiDestroyerOptionalMoqDimensions{}.Destroy(r.Coded)
	FfiDestroyerOptionalMoqDimensions{}.Destroy(r.DisplayAspect)
	FfiDestroyerOptionalUint64{}.Destroy(r.Bitrate)
	FfiDestroyerOptionalFloat64{}.Destroy(r.Framerate)
	FfiDestroyerOptionalBool{}.Destroy(r.OptimizeForLatency)
}

type FfiConverterMoqVideoHint struct{}

var FfiConverterMoqVideoHintINSTANCE = FfiConverterMoqVideoHint{}

func (c FfiConverterMoqVideoHint) Lift(rb RustBufferI) MoqVideoHint {
	return LiftFromRustBuffer[MoqVideoHint](c, rb)
}

func (c FfiConverterMoqVideoHint) Read(reader io.Reader) MoqVideoHint {
	return MoqVideoHint{
		FfiConverterOptionalMoqDimensionsINSTANCE.Read(reader),
		FfiConverterOptionalMoqDimensionsINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalFloat64INSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqVideoHint) Lower(value MoqVideoHint) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideoHint](c, value)
}

func (c FfiConverterMoqVideoHint) LowerExternal(value MoqVideoHint) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideoHint](c, value))
}

func (c FfiConverterMoqVideoHint) Write(writer io.Writer, value MoqVideoHint) {
	FfiConverterOptionalMoqDimensionsINSTANCE.Write(writer, value.Coded)
	FfiConverterOptionalMoqDimensionsINSTANCE.Write(writer, value.DisplayAspect)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.Bitrate)
	FfiConverterOptionalFloat64INSTANCE.Write(writer, value.Framerate)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.OptimizeForLatency)
}

type FfiDestroyerMoqVideoHint struct{}

func (_ FfiDestroyerMoqVideoHint) Destroy(value MoqVideoHint) {
	value.Destroy()
}

// Catalog properties shared by every video rendition.
//
// Passing an absent field clears it from the next catalog snapshot rather than preserving the previous value.
type MoqVideoProperties struct {
	// Final rendered size after rotation, or absent to clear the explicit display size.
	Display *MoqDimensions
	// Clockwise rotation in degrees, or absent to clear the explicit rotation.
	Rotation *float64
	// Whether to flip horizontally after rotation, or absent to clear the explicit value.
	Flip *bool
}

func (r *MoqVideoProperties) Destroy() {
	FfiDestroyerOptionalMoqDimensions{}.Destroy(r.Display)
	FfiDestroyerOptionalFloat64{}.Destroy(r.Rotation)
	FfiDestroyerOptionalBool{}.Destroy(r.Flip)
}

type FfiConverterMoqVideoProperties struct{}

var FfiConverterMoqVideoPropertiesINSTANCE = FfiConverterMoqVideoProperties{}

func (c FfiConverterMoqVideoProperties) Lift(rb RustBufferI) MoqVideoProperties {
	return LiftFromRustBuffer[MoqVideoProperties](c, rb)
}

func (c FfiConverterMoqVideoProperties) Read(reader io.Reader) MoqVideoProperties {
	return MoqVideoProperties{
		FfiConverterOptionalMoqDimensionsINSTANCE.Read(reader),
		FfiConverterOptionalFloat64INSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMoqVideoProperties) Lower(value MoqVideoProperties) C.RustBuffer {
	return LowerIntoRustBuffer[MoqVideoProperties](c, value)
}

func (c FfiConverterMoqVideoProperties) LowerExternal(value MoqVideoProperties) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqVideoProperties](c, value))
}

func (c FfiConverterMoqVideoProperties) Write(writer io.Writer, value MoqVideoProperties) {
	FfiConverterOptionalMoqDimensionsINSTANCE.Write(writer, value.Display)
	FfiConverterOptionalFloat64INSTANCE.Write(writer, value.Rotation)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.Flip)
}

type FfiDestroyerMoqVideoProperties struct{}

func (_ FfiDestroyerMoqVideoProperties) Destroy(value MoqVideoProperties) {
	value.Destroy()
}

// Audio codec identifier.
type MoqAudioCodec uint

const (
	MoqAudioCodecOpus MoqAudioCodec = 1
)

type FfiConverterMoqAudioCodec struct{}

var FfiConverterMoqAudioCodecINSTANCE = FfiConverterMoqAudioCodec{}

func (c FfiConverterMoqAudioCodec) Lift(rb RustBufferI) MoqAudioCodec {
	return LiftFromRustBuffer[MoqAudioCodec](c, rb)
}

func (c FfiConverterMoqAudioCodec) Lower(value MoqAudioCodec) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudioCodec](c, value)
}

func (c FfiConverterMoqAudioCodec) LowerExternal(value MoqAudioCodec) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudioCodec](c, value))
}
func (FfiConverterMoqAudioCodec) Read(reader io.Reader) MoqAudioCodec {
	id := readInt32(reader)
	return MoqAudioCodec(id)
}

func (FfiConverterMoqAudioCodec) Write(writer io.Writer, value MoqAudioCodec) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerMoqAudioCodec struct{}

func (_ FfiDestroyerMoqAudioCodec) Destroy(value MoqAudioCodec) {
}

// Raw PCM sample format, mirroring WebCodecs `AudioData.format`.
//
// <https://developer.mozilla.org/en-US/docs/Web/API/AudioData/format>
type MoqAudioFormat uint

const (
	MoqAudioFormatU8        MoqAudioFormat = 1
	MoqAudioFormatS16       MoqAudioFormat = 2
	MoqAudioFormatS32       MoqAudioFormat = 3
	MoqAudioFormatF32       MoqAudioFormat = 4
	MoqAudioFormatU8Planar  MoqAudioFormat = 5
	MoqAudioFormatS16Planar MoqAudioFormat = 6
	MoqAudioFormatS32Planar MoqAudioFormat = 7
	MoqAudioFormatF32Planar MoqAudioFormat = 8
)

type FfiConverterMoqAudioFormat struct{}

var FfiConverterMoqAudioFormatINSTANCE = FfiConverterMoqAudioFormat{}

func (c FfiConverterMoqAudioFormat) Lift(rb RustBufferI) MoqAudioFormat {
	return LiftFromRustBuffer[MoqAudioFormat](c, rb)
}

func (c FfiConverterMoqAudioFormat) Lower(value MoqAudioFormat) C.RustBuffer {
	return LowerIntoRustBuffer[MoqAudioFormat](c, value)
}

func (c FfiConverterMoqAudioFormat) LowerExternal(value MoqAudioFormat) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqAudioFormat](c, value))
}
func (FfiConverterMoqAudioFormat) Read(reader io.Reader) MoqAudioFormat {
	id := readInt32(reader)
	return MoqAudioFormat(id)
}

func (FfiConverterMoqAudioFormat) Write(writer io.Writer, value MoqAudioFormat) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerMoqAudioFormat struct{}

func (_ FfiDestroyerMoqAudioFormat) Destroy(value MoqAudioFormat) {
}

// How a track's frames are packaged, as advertised in the catalog.
type MoqContainer interface {
	Destroy()
}

// The legacy hang container.
type MoqContainerLegacy struct {
}

func (e MoqContainerLegacy) Destroy() {
}

// CMAF (fMP4), carrying the initialization segment.
type MoqContainerCmaf struct {
	Init []byte
}

func (e MoqContainerCmaf) Destroy() {
	FfiDestroyerBytes{}.Destroy(e.Init)
}

// LOC, the low-overhead container.
type MoqContainerLoc struct {
}

func (e MoqContainerLoc) Destroy() {
}

type FfiConverterMoqContainer struct{}

var FfiConverterMoqContainerINSTANCE = FfiConverterMoqContainer{}

func (c FfiConverterMoqContainer) Lift(rb RustBufferI) MoqContainer {
	return LiftFromRustBuffer[MoqContainer](c, rb)
}

func (c FfiConverterMoqContainer) Lower(value MoqContainer) C.RustBuffer {
	return LowerIntoRustBuffer[MoqContainer](c, value)
}

func (c FfiConverterMoqContainer) LowerExternal(value MoqContainer) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[MoqContainer](c, value))
}
func (FfiConverterMoqContainer) Read(reader io.Reader) MoqContainer {
	id := readInt32(reader)
	switch id {
	case 1:
		return MoqContainerLegacy{}
	case 2:
		return MoqContainerCmaf{
			FfiConverterBytesINSTANCE.Read(reader),
		}
	case 3:
		return MoqContainerLoc{}
	default:
		panic(fmt.Sprintf("invalid enum value %v in FfiConverterMoqContainer.Read()", id))
	}
}

func (FfiConverterMoqContainer) Write(writer io.Writer, value MoqContainer) {
	switch variant_value := value.(type) {
	case MoqContainerLegacy:
		writeInt32(writer, 1)
	case MoqContainerCmaf:
		writeInt32(writer, 2)
		FfiConverterBytesINSTANCE.Write(writer, variant_value.Init)
	case MoqContainerLoc:
		writeInt32(writer, 3)
	default:
		_ = variant_value
		panic(fmt.Sprintf("invalid enum value `%v` in FfiConverterMoqContainer.Write", value))
	}
}

type FfiDestroyerMoqContainer struct{}

func (_ FfiDestroyerMoqContainer) Destroy(value MoqContainer) {
	value.Destroy()
}

// Error returned by all UniFFI-exported functions.
type MoqError struct {
	err error
}

// Convenience method to turn *MoqError into error
// Avoiding treating nil pointer as non nil error interface
func (err *MoqError) AsError() error {
	if err == nil {
		return nil
	} else {
		return err
	}
}

func (err MoqError) Error() string {
	return fmt.Sprintf("MoqError: %s", err.err.Error())
}

func (err MoqError) Unwrap() error {
	return err.err
}

// Err* are used for checking error type with `errors.Is`
var ErrMoqErrorProtocol = fmt.Errorf("MoqErrorProtocol")
var ErrMoqErrorMedia = fmt.Errorf("MoqErrorMedia")
var ErrMoqErrorMux = fmt.Errorf("MoqErrorMux")
var ErrMoqErrorJsonTrack = fmt.Errorf("MoqErrorJsonTrack")
var ErrMoqErrorAudio = fmt.Errorf("MoqErrorAudio")
var ErrMoqErrorUrl = fmt.Errorf("MoqErrorUrl")
var ErrMoqErrorTimeOverflow = fmt.Errorf("MoqErrorTimeOverflow")
var ErrMoqErrorLogLevel = fmt.Errorf("MoqErrorLogLevel")
var ErrMoqErrorTask = fmt.Errorf("MoqErrorTask")
var ErrMoqErrorJson = fmt.Errorf("MoqErrorJson")
var ErrMoqErrorCancelled = fmt.Errorf("MoqErrorCancelled")
var ErrMoqErrorClosed = fmt.Errorf("MoqErrorClosed")
var ErrMoqErrorConnect = fmt.Errorf("MoqErrorConnect")
var ErrMoqErrorBind = fmt.Errorf("MoqErrorBind")
var ErrMoqErrorReject = fmt.Errorf("MoqErrorReject")
var ErrMoqErrorAlreadyResponded = fmt.Errorf("MoqErrorAlreadyResponded")
var ErrMoqErrorCodec = fmt.Errorf("MoqErrorCodec")
var ErrMoqErrorUnauthorized = fmt.Errorf("MoqErrorUnauthorized")
var ErrMoqErrorForbidden = fmt.Errorf("MoqErrorForbidden")
var ErrMoqErrorNotFound = fmt.Errorf("MoqErrorNotFound")
var ErrMoqErrorUnsupported = fmt.Errorf("MoqErrorUnsupported")
var ErrMoqErrorInvalidRoute = fmt.Errorf("MoqErrorInvalidRoute")
var ErrMoqErrorLog = fmt.Errorf("MoqErrorLog")

// Variant structs
type MoqErrorProtocol struct {
	message string
}

func NewMoqErrorProtocol() *MoqError {
	return &MoqError{err: &MoqErrorProtocol{}}
}

func (e MoqErrorProtocol) destroy() {
}

func (err MoqErrorProtocol) Error() string {
	return fmt.Sprintf("Protocol: %s", err.message)
}

func (self MoqErrorProtocol) Is(target error) bool {
	return target == ErrMoqErrorProtocol
}

type MoqErrorMedia struct {
	message string
}

func NewMoqErrorMedia() *MoqError {
	return &MoqError{err: &MoqErrorMedia{}}
}

func (e MoqErrorMedia) destroy() {
}

func (err MoqErrorMedia) Error() string {
	return fmt.Sprintf("Media: %s", err.message)
}

func (self MoqErrorMedia) Is(target error) bool {
	return target == ErrMoqErrorMedia
}

type MoqErrorMux struct {
	message string
}

func NewMoqErrorMux() *MoqError {
	return &MoqError{err: &MoqErrorMux{}}
}

func (e MoqErrorMux) destroy() {
}

func (err MoqErrorMux) Error() string {
	return fmt.Sprintf("Mux: %s", err.message)
}

func (self MoqErrorMux) Is(target error) bool {
	return target == ErrMoqErrorMux
}

type MoqErrorJsonTrack struct {
	message string
}

func NewMoqErrorJsonTrack() *MoqError {
	return &MoqError{err: &MoqErrorJsonTrack{}}
}

func (e MoqErrorJsonTrack) destroy() {
}

func (err MoqErrorJsonTrack) Error() string {
	return fmt.Sprintf("JsonTrack: %s", err.message)
}

func (self MoqErrorJsonTrack) Is(target error) bool {
	return target == ErrMoqErrorJsonTrack
}

type MoqErrorAudio struct {
	message string
}

func NewMoqErrorAudio() *MoqError {
	return &MoqError{err: &MoqErrorAudio{}}
}

func (e MoqErrorAudio) destroy() {
}

func (err MoqErrorAudio) Error() string {
	return fmt.Sprintf("Audio: %s", err.message)
}

func (self MoqErrorAudio) Is(target error) bool {
	return target == ErrMoqErrorAudio
}

type MoqErrorUrl struct {
	message string
}

func NewMoqErrorUrl() *MoqError {
	return &MoqError{err: &MoqErrorUrl{}}
}

func (e MoqErrorUrl) destroy() {
}

func (err MoqErrorUrl) Error() string {
	return fmt.Sprintf("Url: %s", err.message)
}

func (self MoqErrorUrl) Is(target error) bool {
	return target == ErrMoqErrorUrl
}

type MoqErrorTimeOverflow struct {
	message string
}

func NewMoqErrorTimeOverflow() *MoqError {
	return &MoqError{err: &MoqErrorTimeOverflow{}}
}

func (e MoqErrorTimeOverflow) destroy() {
}

func (err MoqErrorTimeOverflow) Error() string {
	return fmt.Sprintf("TimeOverflow: %s", err.message)
}

func (self MoqErrorTimeOverflow) Is(target error) bool {
	return target == ErrMoqErrorTimeOverflow
}

type MoqErrorLogLevel struct {
	message string
}

func NewMoqErrorLogLevel() *MoqError {
	return &MoqError{err: &MoqErrorLogLevel{}}
}

func (e MoqErrorLogLevel) destroy() {
}

func (err MoqErrorLogLevel) Error() string {
	return fmt.Sprintf("LogLevel: %s", err.message)
}

func (self MoqErrorLogLevel) Is(target error) bool {
	return target == ErrMoqErrorLogLevel
}

type MoqErrorTask struct {
	message string
}

func NewMoqErrorTask() *MoqError {
	return &MoqError{err: &MoqErrorTask{}}
}

func (e MoqErrorTask) destroy() {
}

func (err MoqErrorTask) Error() string {
	return fmt.Sprintf("Task: %s", err.message)
}

func (self MoqErrorTask) Is(target error) bool {
	return target == ErrMoqErrorTask
}

type MoqErrorJson struct {
	message string
}

func NewMoqErrorJson() *MoqError {
	return &MoqError{err: &MoqErrorJson{}}
}

func (e MoqErrorJson) destroy() {
}

func (err MoqErrorJson) Error() string {
	return fmt.Sprintf("Json: %s", err.message)
}

func (self MoqErrorJson) Is(target error) bool {
	return target == ErrMoqErrorJson
}

type MoqErrorCancelled struct {
	message string
}

func NewMoqErrorCancelled() *MoqError {
	return &MoqError{err: &MoqErrorCancelled{}}
}

func (e MoqErrorCancelled) destroy() {
}

func (err MoqErrorCancelled) Error() string {
	return fmt.Sprintf("Cancelled: %s", err.message)
}

func (self MoqErrorCancelled) Is(target error) bool {
	return target == ErrMoqErrorCancelled
}

type MoqErrorClosed struct {
	message string
}

func NewMoqErrorClosed() *MoqError {
	return &MoqError{err: &MoqErrorClosed{}}
}

func (e MoqErrorClosed) destroy() {
}

func (err MoqErrorClosed) Error() string {
	return fmt.Sprintf("Closed: %s", err.message)
}

func (self MoqErrorClosed) Is(target error) bool {
	return target == ErrMoqErrorClosed
}

type MoqErrorConnect struct {
	message string
}

func NewMoqErrorConnect() *MoqError {
	return &MoqError{err: &MoqErrorConnect{}}
}

func (e MoqErrorConnect) destroy() {
}

func (err MoqErrorConnect) Error() string {
	return fmt.Sprintf("Connect: %s", err.message)
}

func (self MoqErrorConnect) Is(target error) bool {
	return target == ErrMoqErrorConnect
}

type MoqErrorBind struct {
	message string
}

func NewMoqErrorBind() *MoqError {
	return &MoqError{err: &MoqErrorBind{}}
}

func (e MoqErrorBind) destroy() {
}

func (err MoqErrorBind) Error() string {
	return fmt.Sprintf("Bind: %s", err.message)
}

func (self MoqErrorBind) Is(target error) bool {
	return target == ErrMoqErrorBind
}

type MoqErrorReject struct {
	message string
}

func NewMoqErrorReject() *MoqError {
	return &MoqError{err: &MoqErrorReject{}}
}

func (e MoqErrorReject) destroy() {
}

func (err MoqErrorReject) Error() string {
	return fmt.Sprintf("Reject: %s", err.message)
}

func (self MoqErrorReject) Is(target error) bool {
	return target == ErrMoqErrorReject
}

type MoqErrorAlreadyResponded struct {
	message string
}

func NewMoqErrorAlreadyResponded() *MoqError {
	return &MoqError{err: &MoqErrorAlreadyResponded{}}
}

func (e MoqErrorAlreadyResponded) destroy() {
}

func (err MoqErrorAlreadyResponded) Error() string {
	return fmt.Sprintf("AlreadyResponded: %s", err.message)
}

func (self MoqErrorAlreadyResponded) Is(target error) bool {
	return target == ErrMoqErrorAlreadyResponded
}

type MoqErrorCodec struct {
	message string
}

func NewMoqErrorCodec() *MoqError {
	return &MoqError{err: &MoqErrorCodec{}}
}

func (e MoqErrorCodec) destroy() {
}

func (err MoqErrorCodec) Error() string {
	return fmt.Sprintf("Codec: %s", err.message)
}

func (self MoqErrorCodec) Is(target error) bool {
	return target == ErrMoqErrorCodec
}

type MoqErrorUnauthorized struct {
	message string
}

func NewMoqErrorUnauthorized() *MoqError {
	return &MoqError{err: &MoqErrorUnauthorized{}}
}

func (e MoqErrorUnauthorized) destroy() {
}

func (err MoqErrorUnauthorized) Error() string {
	return fmt.Sprintf("Unauthorized: %s", err.message)
}

func (self MoqErrorUnauthorized) Is(target error) bool {
	return target == ErrMoqErrorUnauthorized
}

type MoqErrorForbidden struct {
	message string
}

func NewMoqErrorForbidden() *MoqError {
	return &MoqError{err: &MoqErrorForbidden{}}
}

func (e MoqErrorForbidden) destroy() {
}

func (err MoqErrorForbidden) Error() string {
	return fmt.Sprintf("Forbidden: %s", err.message)
}

func (self MoqErrorForbidden) Is(target error) bool {
	return target == ErrMoqErrorForbidden
}

// The requested track or group is not available.
type MoqErrorNotFound struct {
	message string
}

// The requested track or group is not available.
func NewMoqErrorNotFound() *MoqError {
	return &MoqError{err: &MoqErrorNotFound{}}
}

func (e MoqErrorNotFound) destroy() {
}

func (err MoqErrorNotFound) Error() string {
	return fmt.Sprintf("NotFound: %s", err.message)
}

func (self MoqErrorNotFound) Is(target error) bool {
	return target == ErrMoqErrorNotFound
}

// The requested operation is not supported.
type MoqErrorUnsupported struct {
	message string
}

// The requested operation is not supported.
func NewMoqErrorUnsupported() *MoqError {
	return &MoqError{err: &MoqErrorUnsupported{}}
}

func (e MoqErrorUnsupported) destroy() {
}

func (err MoqErrorUnsupported) Error() string {
	return fmt.Sprintf("Unsupported: %s", err.message)
}

func (self MoqErrorUnsupported) Is(target error) bool {
	return target == ErrMoqErrorUnsupported
}

// A route carried an invalid hop id or too many hops.
type MoqErrorInvalidRoute struct {
	message string
}

// A route carried an invalid hop id or too many hops.
func NewMoqErrorInvalidRoute() *MoqError {
	return &MoqError{err: &MoqErrorInvalidRoute{}}
}

func (e MoqErrorInvalidRoute) destroy() {
}

func (err MoqErrorInvalidRoute) Error() string {
	return fmt.Sprintf("InvalidRoute: %s", err.message)
}

func (self MoqErrorInvalidRoute) Is(target error) bool {
	return target == ErrMoqErrorInvalidRoute
}

type MoqErrorLog struct {
	message string
}

func NewMoqErrorLog() *MoqError {
	return &MoqError{err: &MoqErrorLog{}}
}

func (e MoqErrorLog) destroy() {
}

func (err MoqErrorLog) Error() string {
	return fmt.Sprintf("Log: %s", err.message)
}

func (self MoqErrorLog) Is(target error) bool {
	return target == ErrMoqErrorLog
}

type FfiConverterMoqError struct{}

var FfiConverterMoqErrorINSTANCE = FfiConverterMoqError{}

func (c FfiConverterMoqError) Lift(eb RustBufferI) *MoqError {
	return LiftFromRustBuffer[*MoqError](c, eb)
}

func (c FfiConverterMoqError) Lower(value *MoqError) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqError](c, value)
}

func (c FfiConverterMoqError) LowerExternal(value *MoqError) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqError](c, value))
}

func (c FfiConverterMoqError) Read(reader io.Reader) *MoqError {
	errorID := readUint32(reader)

	message := FfiConverterStringINSTANCE.Read(reader)
	switch errorID {
	case 1:
		return &MoqError{&MoqErrorProtocol{message}}
	case 2:
		return &MoqError{&MoqErrorMedia{message}}
	case 3:
		return &MoqError{&MoqErrorMux{message}}
	case 4:
		return &MoqError{&MoqErrorJsonTrack{message}}
	case 5:
		return &MoqError{&MoqErrorAudio{message}}
	case 6:
		return &MoqError{&MoqErrorUrl{message}}
	case 7:
		return &MoqError{&MoqErrorTimeOverflow{message}}
	case 8:
		return &MoqError{&MoqErrorLogLevel{message}}
	case 9:
		return &MoqError{&MoqErrorTask{message}}
	case 10:
		return &MoqError{&MoqErrorJson{message}}
	case 11:
		return &MoqError{&MoqErrorCancelled{message}}
	case 12:
		return &MoqError{&MoqErrorClosed{message}}
	case 13:
		return &MoqError{&MoqErrorConnect{message}}
	case 14:
		return &MoqError{&MoqErrorBind{message}}
	case 15:
		return &MoqError{&MoqErrorReject{message}}
	case 16:
		return &MoqError{&MoqErrorAlreadyResponded{message}}
	case 17:
		return &MoqError{&MoqErrorCodec{message}}
	case 18:
		return &MoqError{&MoqErrorUnauthorized{message}}
	case 19:
		return &MoqError{&MoqErrorForbidden{message}}
	case 20:
		return &MoqError{&MoqErrorNotFound{message}}
	case 21:
		return &MoqError{&MoqErrorUnsupported{message}}
	case 22:
		return &MoqError{&MoqErrorInvalidRoute{message}}
	case 23:
		return &MoqError{&MoqErrorLog{message}}
	default:
		panic(fmt.Sprintf("Unknown error code %d in FfiConverterMoqError.Read()", errorID))
	}

}

func (c FfiConverterMoqError) Write(writer io.Writer, value *MoqError) {
	switch variantValue := value.err.(type) {
	case *MoqErrorProtocol:
		writeInt32(writer, 1)
	case *MoqErrorMedia:
		writeInt32(writer, 2)
	case *MoqErrorMux:
		writeInt32(writer, 3)
	case *MoqErrorJsonTrack:
		writeInt32(writer, 4)
	case *MoqErrorAudio:
		writeInt32(writer, 5)
	case *MoqErrorUrl:
		writeInt32(writer, 6)
	case *MoqErrorTimeOverflow:
		writeInt32(writer, 7)
	case *MoqErrorLogLevel:
		writeInt32(writer, 8)
	case *MoqErrorTask:
		writeInt32(writer, 9)
	case *MoqErrorJson:
		writeInt32(writer, 10)
	case *MoqErrorCancelled:
		writeInt32(writer, 11)
	case *MoqErrorClosed:
		writeInt32(writer, 12)
	case *MoqErrorConnect:
		writeInt32(writer, 13)
	case *MoqErrorBind:
		writeInt32(writer, 14)
	case *MoqErrorReject:
		writeInt32(writer, 15)
	case *MoqErrorAlreadyResponded:
		writeInt32(writer, 16)
	case *MoqErrorCodec:
		writeInt32(writer, 17)
	case *MoqErrorUnauthorized:
		writeInt32(writer, 18)
	case *MoqErrorForbidden:
		writeInt32(writer, 19)
	case *MoqErrorNotFound:
		writeInt32(writer, 20)
	case *MoqErrorUnsupported:
		writeInt32(writer, 21)
	case *MoqErrorInvalidRoute:
		writeInt32(writer, 22)
	case *MoqErrorLog:
		writeInt32(writer, 23)
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiConverterMoqError.Write", value))
	}
}

type FfiDestroyerMoqError struct{}

func (_ FfiDestroyerMoqError) Destroy(value *MoqError) {
	switch variantValue := value.err.(type) {
	case MoqErrorProtocol:
		variantValue.destroy()
	case MoqErrorMedia:
		variantValue.destroy()
	case MoqErrorMux:
		variantValue.destroy()
	case MoqErrorJsonTrack:
		variantValue.destroy()
	case MoqErrorAudio:
		variantValue.destroy()
	case MoqErrorUrl:
		variantValue.destroy()
	case MoqErrorTimeOverflow:
		variantValue.destroy()
	case MoqErrorLogLevel:
		variantValue.destroy()
	case MoqErrorTask:
		variantValue.destroy()
	case MoqErrorJson:
		variantValue.destroy()
	case MoqErrorCancelled:
		variantValue.destroy()
	case MoqErrorClosed:
		variantValue.destroy()
	case MoqErrorConnect:
		variantValue.destroy()
	case MoqErrorBind:
		variantValue.destroy()
	case MoqErrorReject:
		variantValue.destroy()
	case MoqErrorAlreadyResponded:
		variantValue.destroy()
	case MoqErrorCodec:
		variantValue.destroy()
	case MoqErrorUnauthorized:
		variantValue.destroy()
	case MoqErrorForbidden:
		variantValue.destroy()
	case MoqErrorNotFound:
		variantValue.destroy()
	case MoqErrorUnsupported:
		variantValue.destroy()
	case MoqErrorInvalidRoute:
		variantValue.destroy()
	case MoqErrorLog:
		variantValue.destroy()
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiDestroyerMoqError.Destroy", value))
	}
}

type FfiConverterOptionalUint32 struct{}

var FfiConverterOptionalUint32INSTANCE = FfiConverterOptionalUint32{}

func (c FfiConverterOptionalUint32) Lift(rb RustBufferI) *uint32 {
	return LiftFromRustBuffer[*uint32](c, rb)
}

func (_ FfiConverterOptionalUint32) Read(reader io.Reader) *uint32 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterUint32INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalUint32) Lower(value *uint32) C.RustBuffer {
	return LowerIntoRustBuffer[*uint32](c, value)
}

func (c FfiConverterOptionalUint32) LowerExternal(value *uint32) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*uint32](c, value))
}

func (_ FfiConverterOptionalUint32) Write(writer io.Writer, value *uint32) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterUint32INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalUint32 struct{}

func (_ FfiDestroyerOptionalUint32) Destroy(value *uint32) {
	if value != nil {
		FfiDestroyerUint32{}.Destroy(*value)
	}
}

type FfiConverterOptionalUint64 struct{}

var FfiConverterOptionalUint64INSTANCE = FfiConverterOptionalUint64{}

func (c FfiConverterOptionalUint64) Lift(rb RustBufferI) *uint64 {
	return LiftFromRustBuffer[*uint64](c, rb)
}

func (_ FfiConverterOptionalUint64) Read(reader io.Reader) *uint64 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterUint64INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalUint64) Lower(value *uint64) C.RustBuffer {
	return LowerIntoRustBuffer[*uint64](c, value)
}

func (c FfiConverterOptionalUint64) LowerExternal(value *uint64) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*uint64](c, value))
}

func (_ FfiConverterOptionalUint64) Write(writer io.Writer, value *uint64) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterUint64INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalUint64 struct{}

func (_ FfiDestroyerOptionalUint64) Destroy(value *uint64) {
	if value != nil {
		FfiDestroyerUint64{}.Destroy(*value)
	}
}

type FfiConverterOptionalFloat64 struct{}

var FfiConverterOptionalFloat64INSTANCE = FfiConverterOptionalFloat64{}

func (c FfiConverterOptionalFloat64) Lift(rb RustBufferI) *float64 {
	return LiftFromRustBuffer[*float64](c, rb)
}

func (_ FfiConverterOptionalFloat64) Read(reader io.Reader) *float64 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterFloat64INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalFloat64) Lower(value *float64) C.RustBuffer {
	return LowerIntoRustBuffer[*float64](c, value)
}

func (c FfiConverterOptionalFloat64) LowerExternal(value *float64) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*float64](c, value))
}

func (_ FfiConverterOptionalFloat64) Write(writer io.Writer, value *float64) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterFloat64INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalFloat64 struct{}

func (_ FfiDestroyerOptionalFloat64) Destroy(value *float64) {
	if value != nil {
		FfiDestroyerFloat64{}.Destroy(*value)
	}
}

type FfiConverterOptionalBool struct{}

var FfiConverterOptionalBoolINSTANCE = FfiConverterOptionalBool{}

func (c FfiConverterOptionalBool) Lift(rb RustBufferI) *bool {
	return LiftFromRustBuffer[*bool](c, rb)
}

func (_ FfiConverterOptionalBool) Read(reader io.Reader) *bool {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterBoolINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalBool) Lower(value *bool) C.RustBuffer {
	return LowerIntoRustBuffer[*bool](c, value)
}

func (c FfiConverterOptionalBool) LowerExternal(value *bool) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*bool](c, value))
}

func (_ FfiConverterOptionalBool) Write(writer io.Writer, value *bool) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterBoolINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalBool struct{}

func (_ FfiDestroyerOptionalBool) Destroy(value *bool) {
	if value != nil {
		FfiDestroyerBool{}.Destroy(*value)
	}
}

type FfiConverterOptionalString struct{}

var FfiConverterOptionalStringINSTANCE = FfiConverterOptionalString{}

func (c FfiConverterOptionalString) Lift(rb RustBufferI) *string {
	return LiftFromRustBuffer[*string](c, rb)
}

func (_ FfiConverterOptionalString) Read(reader io.Reader) *string {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterStringINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalString) Lower(value *string) C.RustBuffer {
	return LowerIntoRustBuffer[*string](c, value)
}

func (c FfiConverterOptionalString) LowerExternal(value *string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*string](c, value))
}

func (_ FfiConverterOptionalString) Write(writer io.Writer, value *string) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalString struct{}

func (_ FfiDestroyerOptionalString) Destroy(value *string) {
	if value != nil {
		FfiDestroyerString{}.Destroy(*value)
	}
}

type FfiConverterOptionalBytes struct{}

var FfiConverterOptionalBytesINSTANCE = FfiConverterOptionalBytes{}

func (c FfiConverterOptionalBytes) Lift(rb RustBufferI) *[]byte {
	return LiftFromRustBuffer[*[]byte](c, rb)
}

func (_ FfiConverterOptionalBytes) Read(reader io.Reader) *[]byte {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterBytesINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalBytes) Lower(value *[]byte) C.RustBuffer {
	return LowerIntoRustBuffer[*[]byte](c, value)
}

func (c FfiConverterOptionalBytes) LowerExternal(value *[]byte) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*[]byte](c, value))
}

func (_ FfiConverterOptionalBytes) Write(writer io.Writer, value *[]byte) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterBytesINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalBytes struct{}

func (_ FfiDestroyerOptionalBytes) Destroy(value *[]byte) {
	if value != nil {
		FfiDestroyerBytes{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqAnnouncement struct{}

var FfiConverterOptionalMoqAnnouncementINSTANCE = FfiConverterOptionalMoqAnnouncement{}

func (c FfiConverterOptionalMoqAnnouncement) Lift(rb RustBufferI) **MoqAnnouncement {
	return LiftFromRustBuffer[**MoqAnnouncement](c, rb)
}

func (_ FfiConverterOptionalMoqAnnouncement) Read(reader io.Reader) **MoqAnnouncement {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqAnnouncementINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqAnnouncement) Lower(value **MoqAnnouncement) C.RustBuffer {
	return LowerIntoRustBuffer[**MoqAnnouncement](c, value)
}

func (c FfiConverterOptionalMoqAnnouncement) LowerExternal(value **MoqAnnouncement) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[**MoqAnnouncement](c, value))
}

func (_ FfiConverterOptionalMoqAnnouncement) Write(writer io.Writer, value **MoqAnnouncement) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqAnnouncementINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqAnnouncement struct{}

func (_ FfiDestroyerOptionalMoqAnnouncement) Destroy(value **MoqAnnouncement) {
	if value != nil {
		FfiDestroyerMoqAnnouncement{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqGroupConsumer struct{}

var FfiConverterOptionalMoqGroupConsumerINSTANCE = FfiConverterOptionalMoqGroupConsumer{}

func (c FfiConverterOptionalMoqGroupConsumer) Lift(rb RustBufferI) **MoqGroupConsumer {
	return LiftFromRustBuffer[**MoqGroupConsumer](c, rb)
}

func (_ FfiConverterOptionalMoqGroupConsumer) Read(reader io.Reader) **MoqGroupConsumer {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqGroupConsumerINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqGroupConsumer) Lower(value **MoqGroupConsumer) C.RustBuffer {
	return LowerIntoRustBuffer[**MoqGroupConsumer](c, value)
}

func (c FfiConverterOptionalMoqGroupConsumer) LowerExternal(value **MoqGroupConsumer) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[**MoqGroupConsumer](c, value))
}

func (_ FfiConverterOptionalMoqGroupConsumer) Write(writer io.Writer, value **MoqGroupConsumer) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqGroupConsumerINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqGroupConsumer struct{}

func (_ FfiDestroyerOptionalMoqGroupConsumer) Destroy(value **MoqGroupConsumer) {
	if value != nil {
		FfiDestroyerMoqGroupConsumer{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqOriginProducer struct{}

var FfiConverterOptionalMoqOriginProducerINSTANCE = FfiConverterOptionalMoqOriginProducer{}

func (c FfiConverterOptionalMoqOriginProducer) Lift(rb RustBufferI) **MoqOriginProducer {
	return LiftFromRustBuffer[**MoqOriginProducer](c, rb)
}

func (_ FfiConverterOptionalMoqOriginProducer) Read(reader io.Reader) **MoqOriginProducer {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqOriginProducerINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqOriginProducer) Lower(value **MoqOriginProducer) C.RustBuffer {
	return LowerIntoRustBuffer[**MoqOriginProducer](c, value)
}

func (c FfiConverterOptionalMoqOriginProducer) LowerExternal(value **MoqOriginProducer) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[**MoqOriginProducer](c, value))
}

func (_ FfiConverterOptionalMoqOriginProducer) Write(writer io.Writer, value **MoqOriginProducer) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqOriginProducerINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqOriginProducer struct{}

func (_ FfiDestroyerOptionalMoqOriginProducer) Destroy(value **MoqOriginProducer) {
	if value != nil {
		FfiDestroyerMoqOriginProducer{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqRequest struct{}

var FfiConverterOptionalMoqRequestINSTANCE = FfiConverterOptionalMoqRequest{}

func (c FfiConverterOptionalMoqRequest) Lift(rb RustBufferI) **MoqRequest {
	return LiftFromRustBuffer[**MoqRequest](c, rb)
}

func (_ FfiConverterOptionalMoqRequest) Read(reader io.Reader) **MoqRequest {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqRequestINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqRequest) Lower(value **MoqRequest) C.RustBuffer {
	return LowerIntoRustBuffer[**MoqRequest](c, value)
}

func (c FfiConverterOptionalMoqRequest) LowerExternal(value **MoqRequest) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[**MoqRequest](c, value))
}

func (_ FfiConverterOptionalMoqRequest) Write(writer io.Writer, value **MoqRequest) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqRequestINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqRequest struct{}

func (_ FfiDestroyerOptionalMoqRequest) Destroy(value **MoqRequest) {
	if value != nil {
		FfiDestroyerMoqRequest{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqAudioFrame struct{}

var FfiConverterOptionalMoqAudioFrameINSTANCE = FfiConverterOptionalMoqAudioFrame{}

func (c FfiConverterOptionalMoqAudioFrame) Lift(rb RustBufferI) *MoqAudioFrame {
	return LiftFromRustBuffer[*MoqAudioFrame](c, rb)
}

func (_ FfiConverterOptionalMoqAudioFrame) Read(reader io.Reader) *MoqAudioFrame {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqAudioFrameINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqAudioFrame) Lower(value *MoqAudioFrame) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqAudioFrame](c, value)
}

func (c FfiConverterOptionalMoqAudioFrame) LowerExternal(value *MoqAudioFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqAudioFrame](c, value))
}

func (_ FfiConverterOptionalMoqAudioFrame) Write(writer io.Writer, value *MoqAudioFrame) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqAudioFrameINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqAudioFrame struct{}

func (_ FfiDestroyerOptionalMoqAudioFrame) Destroy(value *MoqAudioFrame) {
	if value != nil {
		FfiDestroyerMoqAudioFrame{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqCatalog struct{}

var FfiConverterOptionalMoqCatalogINSTANCE = FfiConverterOptionalMoqCatalog{}

func (c FfiConverterOptionalMoqCatalog) Lift(rb RustBufferI) *MoqCatalog {
	return LiftFromRustBuffer[*MoqCatalog](c, rb)
}

func (_ FfiConverterOptionalMoqCatalog) Read(reader io.Reader) *MoqCatalog {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqCatalogINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqCatalog) Lower(value *MoqCatalog) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqCatalog](c, value)
}

func (c FfiConverterOptionalMoqCatalog) LowerExternal(value *MoqCatalog) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqCatalog](c, value))
}

func (_ FfiConverterOptionalMoqCatalog) Write(writer io.Writer, value *MoqCatalog) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqCatalogINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqCatalog struct{}

func (_ FfiDestroyerOptionalMoqCatalog) Destroy(value *MoqCatalog) {
	if value != nil {
		FfiDestroyerMoqCatalog{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqDatagram struct{}

var FfiConverterOptionalMoqDatagramINSTANCE = FfiConverterOptionalMoqDatagram{}

func (c FfiConverterOptionalMoqDatagram) Lift(rb RustBufferI) *MoqDatagram {
	return LiftFromRustBuffer[*MoqDatagram](c, rb)
}

func (_ FfiConverterOptionalMoqDatagram) Read(reader io.Reader) *MoqDatagram {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqDatagramINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqDatagram) Lower(value *MoqDatagram) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqDatagram](c, value)
}

func (c FfiConverterOptionalMoqDatagram) LowerExternal(value *MoqDatagram) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqDatagram](c, value))
}

func (_ FfiConverterOptionalMoqDatagram) Write(writer io.Writer, value *MoqDatagram) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqDatagramINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqDatagram struct{}

func (_ FfiDestroyerOptionalMoqDatagram) Destroy(value *MoqDatagram) {
	if value != nil {
		FfiDestroyerMoqDatagram{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqDimensions struct{}

var FfiConverterOptionalMoqDimensionsINSTANCE = FfiConverterOptionalMoqDimensions{}

func (c FfiConverterOptionalMoqDimensions) Lift(rb RustBufferI) *MoqDimensions {
	return LiftFromRustBuffer[*MoqDimensions](c, rb)
}

func (_ FfiConverterOptionalMoqDimensions) Read(reader io.Reader) *MoqDimensions {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqDimensionsINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqDimensions) Lower(value *MoqDimensions) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqDimensions](c, value)
}

func (c FfiConverterOptionalMoqDimensions) LowerExternal(value *MoqDimensions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqDimensions](c, value))
}

func (_ FfiConverterOptionalMoqDimensions) Write(writer io.Writer, value *MoqDimensions) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqDimensionsINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqDimensions struct{}

func (_ FfiDestroyerOptionalMoqDimensions) Destroy(value *MoqDimensions) {
	if value != nil {
		FfiDestroyerMoqDimensions{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqFetchGroupOptions struct{}

var FfiConverterOptionalMoqFetchGroupOptionsINSTANCE = FfiConverterOptionalMoqFetchGroupOptions{}

func (c FfiConverterOptionalMoqFetchGroupOptions) Lift(rb RustBufferI) *MoqFetchGroupOptions {
	return LiftFromRustBuffer[*MoqFetchGroupOptions](c, rb)
}

func (_ FfiConverterOptionalMoqFetchGroupOptions) Read(reader io.Reader) *MoqFetchGroupOptions {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqFetchGroupOptionsINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqFetchGroupOptions) Lower(value *MoqFetchGroupOptions) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqFetchGroupOptions](c, value)
}

func (c FfiConverterOptionalMoqFetchGroupOptions) LowerExternal(value *MoqFetchGroupOptions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqFetchGroupOptions](c, value))
}

func (_ FfiConverterOptionalMoqFetchGroupOptions) Write(writer io.Writer, value *MoqFetchGroupOptions) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqFetchGroupOptionsINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqFetchGroupOptions struct{}

func (_ FfiDestroyerOptionalMoqFetchGroupOptions) Destroy(value *MoqFetchGroupOptions) {
	if value != nil {
		FfiDestroyerMoqFetchGroupOptions{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqFrame struct{}

var FfiConverterOptionalMoqFrameINSTANCE = FfiConverterOptionalMoqFrame{}

func (c FfiConverterOptionalMoqFrame) Lift(rb RustBufferI) *MoqFrame {
	return LiftFromRustBuffer[*MoqFrame](c, rb)
}

func (_ FfiConverterOptionalMoqFrame) Read(reader io.Reader) *MoqFrame {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqFrameINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqFrame) Lower(value *MoqFrame) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqFrame](c, value)
}

func (c FfiConverterOptionalMoqFrame) LowerExternal(value *MoqFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqFrame](c, value))
}

func (_ FfiConverterOptionalMoqFrame) Write(writer io.Writer, value *MoqFrame) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqFrameINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqFrame struct{}

func (_ FfiDestroyerOptionalMoqFrame) Destroy(value *MoqFrame) {
	if value != nil {
		FfiDestroyerMoqFrame{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqMediaFrame struct{}

var FfiConverterOptionalMoqMediaFrameINSTANCE = FfiConverterOptionalMoqMediaFrame{}

func (c FfiConverterOptionalMoqMediaFrame) Lift(rb RustBufferI) *MoqMediaFrame {
	return LiftFromRustBuffer[*MoqMediaFrame](c, rb)
}

func (_ FfiConverterOptionalMoqMediaFrame) Read(reader io.Reader) *MoqMediaFrame {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqMediaFrameINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqMediaFrame) Lower(value *MoqMediaFrame) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqMediaFrame](c, value)
}

func (c FfiConverterOptionalMoqMediaFrame) LowerExternal(value *MoqMediaFrame) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqMediaFrame](c, value))
}

func (_ FfiConverterOptionalMoqMediaFrame) Write(writer io.Writer, value *MoqMediaFrame) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqMediaFrameINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqMediaFrame struct{}

func (_ FfiDestroyerOptionalMoqMediaFrame) Destroy(value *MoqMediaFrame) {
	if value != nil {
		FfiDestroyerMoqMediaFrame{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqRoute struct{}

var FfiConverterOptionalMoqRouteINSTANCE = FfiConverterOptionalMoqRoute{}

func (c FfiConverterOptionalMoqRoute) Lift(rb RustBufferI) *MoqRoute {
	return LiftFromRustBuffer[*MoqRoute](c, rb)
}

func (_ FfiConverterOptionalMoqRoute) Read(reader io.Reader) *MoqRoute {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqRouteINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqRoute) Lower(value *MoqRoute) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqRoute](c, value)
}

func (c FfiConverterOptionalMoqRoute) LowerExternal(value *MoqRoute) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqRoute](c, value))
}

func (_ FfiConverterOptionalMoqRoute) Write(writer io.Writer, value *MoqRoute) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqRouteINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqRoute struct{}

func (_ FfiDestroyerOptionalMoqRoute) Destroy(value *MoqRoute) {
	if value != nil {
		FfiDestroyerMoqRoute{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqSubscription struct{}

var FfiConverterOptionalMoqSubscriptionINSTANCE = FfiConverterOptionalMoqSubscription{}

func (c FfiConverterOptionalMoqSubscription) Lift(rb RustBufferI) *MoqSubscription {
	return LiftFromRustBuffer[*MoqSubscription](c, rb)
}

func (_ FfiConverterOptionalMoqSubscription) Read(reader io.Reader) *MoqSubscription {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqSubscriptionINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqSubscription) Lower(value *MoqSubscription) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqSubscription](c, value)
}

func (c FfiConverterOptionalMoqSubscription) LowerExternal(value *MoqSubscription) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqSubscription](c, value))
}

func (_ FfiConverterOptionalMoqSubscription) Write(writer io.Writer, value *MoqSubscription) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqSubscriptionINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqSubscription struct{}

func (_ FfiDestroyerOptionalMoqSubscription) Destroy(value *MoqSubscription) {
	if value != nil {
		FfiDestroyerMoqSubscription{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqTrackInfo struct{}

var FfiConverterOptionalMoqTrackInfoINSTANCE = FfiConverterOptionalMoqTrackInfo{}

func (c FfiConverterOptionalMoqTrackInfo) Lift(rb RustBufferI) *MoqTrackInfo {
	return LiftFromRustBuffer[*MoqTrackInfo](c, rb)
}

func (_ FfiConverterOptionalMoqTrackInfo) Read(reader io.Reader) *MoqTrackInfo {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqTrackInfoINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqTrackInfo) Lower(value *MoqTrackInfo) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqTrackInfo](c, value)
}

func (c FfiConverterOptionalMoqTrackInfo) LowerExternal(value *MoqTrackInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqTrackInfo](c, value))
}

func (_ FfiConverterOptionalMoqTrackInfo) Write(writer io.Writer, value *MoqTrackInfo) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqTrackInfoINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqTrackInfo struct{}

func (_ FfiDestroyerOptionalMoqTrackInfo) Destroy(value *MoqTrackInfo) {
	if value != nil {
		FfiDestroyerMoqTrackInfo{}.Destroy(*value)
	}
}

type FfiConverterOptionalMoqVideoHint struct{}

var FfiConverterOptionalMoqVideoHintINSTANCE = FfiConverterOptionalMoqVideoHint{}

func (c FfiConverterOptionalMoqVideoHint) Lift(rb RustBufferI) *MoqVideoHint {
	return LiftFromRustBuffer[*MoqVideoHint](c, rb)
}

func (_ FfiConverterOptionalMoqVideoHint) Read(reader io.Reader) *MoqVideoHint {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterMoqVideoHintINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalMoqVideoHint) Lower(value *MoqVideoHint) C.RustBuffer {
	return LowerIntoRustBuffer[*MoqVideoHint](c, value)
}

func (c FfiConverterOptionalMoqVideoHint) LowerExternal(value *MoqVideoHint) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*MoqVideoHint](c, value))
}

func (_ FfiConverterOptionalMoqVideoHint) Write(writer io.Writer, value *MoqVideoHint) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterMoqVideoHintINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalMoqVideoHint struct{}

func (_ FfiDestroyerOptionalMoqVideoHint) Destroy(value *MoqVideoHint) {
	if value != nil {
		FfiDestroyerMoqVideoHint{}.Destroy(*value)
	}
}

type FfiConverterSequenceUint64 struct{}

var FfiConverterSequenceUint64INSTANCE = FfiConverterSequenceUint64{}

func (c FfiConverterSequenceUint64) Lift(rb RustBufferI) []uint64 {
	return LiftFromRustBuffer[[]uint64](c, rb)
}

func (c FfiConverterSequenceUint64) Read(reader io.Reader) []uint64 {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]uint64, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterUint64INSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceUint64) Lower(value []uint64) C.RustBuffer {
	return LowerIntoRustBuffer[[]uint64](c, value)
}

func (c FfiConverterSequenceUint64) LowerExternal(value []uint64) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]uint64](c, value))
}

func (c FfiConverterSequenceUint64) Write(writer io.Writer, value []uint64) {
	if len(value) > math.MaxInt32 {
		panic("[]uint64 is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterUint64INSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceUint64 struct{}

func (FfiDestroyerSequenceUint64) Destroy(sequence []uint64) {
	for _, value := range sequence {
		FfiDestroyerUint64{}.Destroy(value)
	}
}

type FfiConverterSequenceString struct{}

var FfiConverterSequenceStringINSTANCE = FfiConverterSequenceString{}

func (c FfiConverterSequenceString) Lift(rb RustBufferI) []string {
	return LiftFromRustBuffer[[]string](c, rb)
}

func (c FfiConverterSequenceString) Read(reader io.Reader) []string {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]string, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterStringINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceString) Lower(value []string) C.RustBuffer {
	return LowerIntoRustBuffer[[]string](c, value)
}

func (c FfiConverterSequenceString) LowerExternal(value []string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]string](c, value))
}

func (c FfiConverterSequenceString) Write(writer io.Writer, value []string) {
	if len(value) > math.MaxInt32 {
		panic("[]string is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterStringINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceString struct{}

func (FfiDestroyerSequenceString) Destroy(sequence []string) {
	for _, value := range sequence {
		FfiDestroyerString{}.Destroy(value)
	}
}

type FfiConverterMapStringString struct{}

var FfiConverterMapStringStringINSTANCE = FfiConverterMapStringString{}

func (c FfiConverterMapStringString) Lift(rb RustBufferI) map[string]string {
	return LiftFromRustBuffer[map[string]string](c, rb)
}

func (_ FfiConverterMapStringString) Read(reader io.Reader) map[string]string {
	result := make(map[string]string)
	length := readInt32(reader)
	for i := int32(0); i < length; i++ {
		key := FfiConverterStringINSTANCE.Read(reader)
		value := FfiConverterStringINSTANCE.Read(reader)
		result[key] = value
	}
	return result
}

func (c FfiConverterMapStringString) Lower(value map[string]string) C.RustBuffer {
	return LowerIntoRustBuffer[map[string]string](c, value)
}

func (c FfiConverterMapStringString) LowerExternal(value map[string]string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[map[string]string](c, value))
}

func (_ FfiConverterMapStringString) Write(writer io.Writer, mapValue map[string]string) {
	if len(mapValue) > math.MaxInt32 {
		panic("map[string]string is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(mapValue)))
	for key, value := range mapValue {
		FfiConverterStringINSTANCE.Write(writer, key)
		FfiConverterStringINSTANCE.Write(writer, value)
	}
}

type FfiDestroyerMapStringString struct{}

func (_ FfiDestroyerMapStringString) Destroy(mapValue map[string]string) {
	for key, value := range mapValue {
		FfiDestroyerString{}.Destroy(key)
		FfiDestroyerString{}.Destroy(value)
	}
}

type FfiConverterMapStringMoqAudio struct{}

var FfiConverterMapStringMoqAudioINSTANCE = FfiConverterMapStringMoqAudio{}

func (c FfiConverterMapStringMoqAudio) Lift(rb RustBufferI) map[string]MoqAudio {
	return LiftFromRustBuffer[map[string]MoqAudio](c, rb)
}

func (_ FfiConverterMapStringMoqAudio) Read(reader io.Reader) map[string]MoqAudio {
	result := make(map[string]MoqAudio)
	length := readInt32(reader)
	for i := int32(0); i < length; i++ {
		key := FfiConverterStringINSTANCE.Read(reader)
		value := FfiConverterMoqAudioINSTANCE.Read(reader)
		result[key] = value
	}
	return result
}

func (c FfiConverterMapStringMoqAudio) Lower(value map[string]MoqAudio) C.RustBuffer {
	return LowerIntoRustBuffer[map[string]MoqAudio](c, value)
}

func (c FfiConverterMapStringMoqAudio) LowerExternal(value map[string]MoqAudio) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[map[string]MoqAudio](c, value))
}

func (_ FfiConverterMapStringMoqAudio) Write(writer io.Writer, mapValue map[string]MoqAudio) {
	if len(mapValue) > math.MaxInt32 {
		panic("map[string]MoqAudio is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(mapValue)))
	for key, value := range mapValue {
		FfiConverterStringINSTANCE.Write(writer, key)
		FfiConverterMoqAudioINSTANCE.Write(writer, value)
	}
}

type FfiDestroyerMapStringMoqAudio struct{}

func (_ FfiDestroyerMapStringMoqAudio) Destroy(mapValue map[string]MoqAudio) {
	for key, value := range mapValue {
		FfiDestroyerString{}.Destroy(key)
		FfiDestroyerMoqAudio{}.Destroy(value)
	}
}

type FfiConverterMapStringMoqVideo struct{}

var FfiConverterMapStringMoqVideoINSTANCE = FfiConverterMapStringMoqVideo{}

func (c FfiConverterMapStringMoqVideo) Lift(rb RustBufferI) map[string]MoqVideo {
	return LiftFromRustBuffer[map[string]MoqVideo](c, rb)
}

func (_ FfiConverterMapStringMoqVideo) Read(reader io.Reader) map[string]MoqVideo {
	result := make(map[string]MoqVideo)
	length := readInt32(reader)
	for i := int32(0); i < length; i++ {
		key := FfiConverterStringINSTANCE.Read(reader)
		value := FfiConverterMoqVideoINSTANCE.Read(reader)
		result[key] = value
	}
	return result
}

func (c FfiConverterMapStringMoqVideo) Lower(value map[string]MoqVideo) C.RustBuffer {
	return LowerIntoRustBuffer[map[string]MoqVideo](c, value)
}

func (c FfiConverterMapStringMoqVideo) LowerExternal(value map[string]MoqVideo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[map[string]MoqVideo](c, value))
}

func (_ FfiConverterMapStringMoqVideo) Write(writer io.Writer, mapValue map[string]MoqVideo) {
	if len(mapValue) > math.MaxInt32 {
		panic("map[string]MoqVideo is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(mapValue)))
	for key, value := range mapValue {
		FfiConverterStringINSTANCE.Write(writer, key)
		FfiConverterMoqVideoINSTANCE.Write(writer, value)
	}
}

type FfiDestroyerMapStringMoqVideo struct{}

func (_ FfiDestroyerMapStringMoqVideo) Destroy(mapValue map[string]MoqVideo) {
	for key, value := range mapValue {
		FfiDestroyerString{}.Destroy(key)
		FfiDestroyerMoqVideo{}.Destroy(value)
	}
}

const (
	uniffiRustFuturePollReady      int8 = 0
	uniffiRustFuturePollMaybeReady int8 = 1
)

type rustFuturePollFunc func(C.uint64_t, C.UniffiRustFutureContinuationCallback, C.uint64_t)
type rustFutureCompleteFunc[T any] func(C.uint64_t, *C.RustCallStatus) T
type rustFutureFreeFunc func(C.uint64_t)

//export moq_uniffiFutureContinuationCallback
func moq_uniffiFutureContinuationCallback(data C.uint64_t, pollResult C.int8_t) {
	h := cgo.Handle(uintptr(data))
	waiter := h.Value().(chan int8)
	waiter <- int8(pollResult)
}

func uniffiRustCallAsync[E any, T any, F any](
	errConverter BufReader[E],
	completeFunc rustFutureCompleteFunc[F],
	liftFunc func(F) T,
	rustFuture C.uint64_t,
	pollFunc rustFuturePollFunc,
	freeFunc rustFutureFreeFunc,
) (T, E) {
	defer freeFunc(rustFuture)

	pollResult := int8(-1)
	waiter := make(chan int8, 1)

	chanHandle := cgo.NewHandle(waiter)
	defer chanHandle.Delete()

	for pollResult != uniffiRustFuturePollReady {
		pollFunc(
			rustFuture,
			(C.UniffiRustFutureContinuationCallback)(C.moq_uniffiFutureContinuationCallback),
			C.uint64_t(chanHandle),
		)
		pollResult = <-waiter
	}

	var goValue T
	ffiValue, err := rustCallWithError(errConverter, func(status *C.RustCallStatus) F {
		return completeFunc(rustFuture, status)
	})
	if value := reflect.ValueOf(err); value.IsValid() && !value.IsZero() {
		return goValue, err
	}
	return liftFunc(ffiValue), err
}

//export moq_uniffiFreeGorutine
func moq_uniffiFreeGorutine(data C.uint64_t) {
	handle := cgo.Handle(uintptr(data))
	defer handle.Delete()

	guard := handle.Value().(chan struct{})
	guard <- struct{}{}
}

// Initialize logging with a level string: "error", "warn", "info", "debug", "trace", or "".
//
// Returns an error if called more than once.
func MoqLogLevel(level string) error {
	_, _uniffiErr := rustCallWithError[*MoqError](FfiConverterMoqError{}, func(_uniffiStatus *C.RustCallStatus) bool {
		C.uniffi_moq_ffi_fn_func_moq_log_level(FfiConverterStringINSTANCE.Lower(level), _uniffiStatus)
		return false
	})
	return _uniffiErr.AsError()
}
