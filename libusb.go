// Copyright 2017 the gousb Authors.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gousb

import (
	"errors"
	"fmt"
	"log"
	"reflect"
	"sync"
	"time"
	"unsafe"
)

/*
#cgo pkg-config: libusb-1.0
#include <libusb.h>
#include <stdlib.h>

int gousb_compact_iso_data(struct libusb_transfer *xfer, unsigned char *status);
struct libusb_transfer *gousb_alloc_transfer_and_buffer(int bufLen, int numIsoPackets);
void gousb_free_transfer_and_buffer(struct libusb_transfer *xfer);
int submit(struct libusb_transfer *xfer);
void gousb_set_debug(libusb_context *ctx, int lvl);
int gousb_hotplug_register_callback(
	libusb_context* ctx,
	libusb_hotplug_event events,
	libusb_hotplug_flag flags,
	int vid,
	int pid,
	int dev_class,
	void *user_data,
	libusb_hotplug_callback_handle *handle
);
*/
import "C"

type libusbContext C.libusb_context
type libusbDevice C.libusb_device
type libusbDevHandle C.libusb_device_handle
type libusbTransfer C.struct_libusb_transfer
type libusbEndpoint C.struct_libusb_endpoint_descriptor

func (ep libusbEndpoint) endpointDesc(dev *DeviceDesc) EndpointDesc {
	ei := EndpointDesc{
		Address:       EndpointAddress(ep.bEndpointAddress),
		Number:        int(ep.bEndpointAddress & endpointNumMask),
		Direction:     EndpointDirection((ep.bEndpointAddress & endpointDirectionMask) != 0),
		TransferType:  TransferType(ep.bmAttributes & transferTypeMask),
		MaxPacketSize: int(ep.wMaxPacketSize),
	}
	if ei.TransferType == TransferTypeIsochronous {
		// bits 0-10 identify the packet size, bits 11-12 are the number of additional transactions per microframe.
		// Don't use libusb_get_max_iso_packet_size, as it has a bug where it returns the same value
		// regardless of alternative setting used, where different alternative settings might define different
		// max packet sizes.
		// See http://libusb.org/ticket/77 for more background.
		ei.MaxPacketSize = int(ep.wMaxPacketSize) & 0x07ff * (int(ep.wMaxPacketSize)>>11&3 + 1)
		ei.IsoSyncType = IsoSyncType(ep.bmAttributes & isoSyncTypeMask)
		switch ep.bmAttributes & usageTypeMask {
		case C.LIBUSB_ISO_USAGE_TYPE_DATA:
			ei.UsageType = IsoUsageTypeData
		case C.LIBUSB_ISO_USAGE_TYPE_FEEDBACK:
			ei.UsageType = IsoUsageTypeFeedback
		case C.LIBUSB_ISO_USAGE_TYPE_IMPLICIT:
			ei.UsageType = IsoUsageTypeImplicit
		}
	}
	switch {
	// If the device conforms to USB1.x:
	//   Interval for polling endpoint for data transfers. Expressed in
	//   milliseconds.
	//   This field is ignored for bulk and control endpoints. For
	//   isochronous endpoints this field must be set to 1. For interrupt
	//   endpoints, this field may range from 1 to 255.
	// Note: in low-speed mode, isochronous transfers are not supported.
	case dev.Spec < Version(2, 0):
		ei.PollInterval = time.Duration(ep.bInterval) * time.Millisecond

	// If the device conforms to USB[23].x and the device is in low or full
	// speed mode:
	//   Interval for polling endpoint for data transfers.  Expressed in
	//   frames (1ms)
	//   For full-speed isochronous endpoints, the value of this field should
	//   be 1.
	//   For full-/low-speed interrupt endpoints, the value of this field may
	//   be from 1 to 255.
	// Note: in low-speed mode, isochronous transfers are not supported.
	case dev.Speed == SpeedUnknown || dev.Speed == SpeedLow || dev.Speed == SpeedFull:
		ei.PollInterval = time.Duration(ep.bInterval) * time.Millisecond

	// If the device conforms to USB[23].x and the device is in high speed
	// mode:
	//   Interval is expressed in microframe units (125 µs).
	//   For high-speed bulk/control OUT endpoints, the bInterval must
	//   specify the maximum NAK rate of the endpoint. A value of 0 indicates
	//   the endpoint never NAKs. Other values indicate at most 1 NAK each
	//   bInterval number of microframes. This value must be in the range
	//   from 0 to 255.
	case dev.Speed == SpeedHigh && ei.TransferType == TransferTypeBulk:
		ei.PollInterval = time.Duration(ep.bInterval) * 125 * time.Microsecond

	// If the device conforms to USB[23].x and the device is in high speed
	// mode:
	//   For high-speed isochronous endpoints, this value must be in
	//   the range from 1 to 16. The bInterval value is used as the exponent
	//   for a 2bInterval-1 value; e.g., a bInterval of 4 means a period
	//   of 8 (2^(4-1)).
	//   For high-speed interrupt endpoints, the bInterval value is used as
	//   the exponent for a 2bInterval-1 value; e.g., a bInterval of 4 means
	//   a period of 8 (2^(4-1)). This value must be from 1 to 16.
	// If the device conforms to USB3.x and the device is in SuperSpeed mode:
	//   Interval for servicing the endpoint for data transfers. Expressed in
	//   125-µs units.
	//   For Enhanced SuperSpeed isochronous and interrupt endpoints, this
	//   value shall be in the range from 1 to 16. However, the valid ranges
	//   are 8 to 16 for Notification type Interrupt endpoints. The bInterval
	//   value is used as the exponent for a 2(^bInterval-1) value; e.g., a
	//   bInterval of 4 means a period of 8 (2^(4-1) → 2^3 → 8).
	//   This field is reserved and shall not be used for Enhanced SuperSpeed
	//   bulk or control endpoints.
	case dev.Speed == SpeedHigh || dev.Speed == SpeedSuper:
		ei.PollInterval = 125 * time.Microsecond << (ep.bInterval - 1)
	}
	return ei
}

type libusbHotplugCallback func(*libusbContext, *libusbDevice, HotplugEventType) bool

// libusbIntf is a set of trivial idiomatic Go wrappers around libusb C functions.
// The underlying code is generally not testable or difficult to test,
// since libusb interacts directly with the host USB stack.
//
// All functions here should operate on types defined on C.libusb* data types,
// and occasionally on convenience data types (like TransferType or DeviceDesc).
type libusbIntf interface {
	// context
	init() (*libusbContext, error)
	handleEvents(*libusbContext, <-chan struct{})
	getDevices(*libusbContext) ([]*libusbDevice, error)
	exit(*libusbContext) error
	setDebug(*libusbContext, int)

	// device
	dereference(*libusbDevice)
	getDeviceDesc(*libusbDevice) (*DeviceDesc, error)
	open(*libusbDevice) (*libusbDevHandle, error)
	wrapSysDevice(*libusbContext, uintptr) (*libusbDevHandle, error)

	close(*libusbDevHandle)
	reset(*libusbDevHandle) error
	control(*libusbDevHandle, time.Duration, uint8, uint8, uint16, uint16, []byte) (int, error)
	bulk(d *libusbDevHandle, address EndpointAddress, data []byte, timeout time.Duration) (int, error)
	getConfig(*libusbDevHandle) (uint8, error)
	setConfig(*libusbDevHandle, uint8) error
	getStringDesc(*libusbDevHandle, int) (string, error)
	setAutoDetach(*libusbDevHandle, int) error
	detachKernelDriver(*libusbDevHandle, uint8) error
	getDevice(*libusbDevHandle) *libusbDevice

	// interface
	claim(*libusbDevHandle, uint8) error
	release(*libusbDevHandle, uint8)
	setAlt(*libusbDevHandle, uint8, uint8) error

	// transfer
	alloc(*libusbDevHandle, *EndpointDesc, int, int, chan struct{}) (*libusbTransfer, error)
	cancel(*libusbTransfer) error
	submit(*libusbTransfer) error
	buffer(*libusbTransfer) []byte
	data(*libusbTransfer) (int, TransferStatus)
	free(*libusbTransfer)
	setIsoPacketLengths(*libusbTransfer, uint32)

	// hotplug
	registerHotplugCallback(ctx *libusbContext, events HotplugEventType, enumerate bool, vendorID int32, productID int32, devClass int32, fn libusbHotplugCallback) (func(), error)
}

// libusbImpl is an implementation of libusbIntf using real CGo-wrapped libusb.
type libusbImpl struct {
	discovery DeviceDiscovery
}

func (impl libusbImpl) init() (*libusbContext, error) {
	var ctx *C.libusb_context

	var libusbOpts [4]C.struct_libusb_init_option // fixed to 4 - there are maximum 4 options
	nOpts := 0
	if impl.discovery == DisableDeviceDiscovery {
		libusbOpts[nOpts].option = C.LIBUSB_OPTION_NO_DEVICE_DISCOVERY
		nOpts++
	}

	if err := fromErrNo(C.libusb_init_context(&ctx, &(libusbOpts[0]), C.int(nOpts))); err != nil {
		return nil, err
	}

	return (*libusbContext)(ctx), nil
}

func (libusbImpl) handleEvents(c *libusbContext, done <-chan struct{}) {
	tv := C.struct_timeval{tv_usec: 100e3}
	for {
		select {
		case <-done:
			return
		default:
		}
		if errno := C.libusb_handle_events_timeout_completed((*C.libusb_context)(c), &tv, nil); errno < 0 {
			// handler can be interrupted by a signal and this doesn't indicate an error, we'll retry on the next loop iteration
			if Error(errno) != ErrorInterrupted {
				log.Printf("handle_events: error: %s", Error(errno))
			}
		}
	}
}

func (libusbImpl) getDevices(ctx *libusbContext) ([]*libusbDevice, error) {
	var list **C.libusb_device
	cnt := C.libusb_get_device_list((*C.libusb_context)(ctx), &list)
	if cnt < 0 {
		return nil, fromErrNo(C.int(cnt))
	}
	var devs []*C.libusb_device
	*(*reflect.SliceHeader)(unsafe.Pointer(&devs)) = reflect.SliceHeader{
		Data: uintptr(unsafe.Pointer(list)),
		Len:  int(cnt),
		Cap:  int(cnt),
	}
	var ret []*libusbDevice
	for _, d := range devs {
		ret = append(ret, (*libusbDevice)(d))
	}
	// devices must be dereferenced by the caller to prevent memory leaks.
	C.libusb_free_device_list(list, 0)
	return ret, nil
}

func (libusbImpl) wrapSysDevice(ctx *libusbContext, fd uintptr) (*libusbDevHandle, error) {
	var handle *C.libusb_device_handle
	if ret := C.libusb_wrap_sys_device((*C.libusb_context)(ctx), C.intptr_t(fd), &handle); ret < 0 {
		return nil, fromErrNo(C.int(ret))
	}

	return (*libusbDevHandle)(handle), nil
}

func (libusbImpl) getDevice(d *libusbDevHandle) *libusbDevice {
	device := C.libusb_get_device((*C.libusb_device_handle)(d))
	return (*libusbDevice)(device)
}

func (libusbImpl) exit(c *libusbContext) error {
	C.libusb_exit((*C.libusb_context)(c))
	return nil
}

func (libusbImpl) setDebug(c *libusbContext, lvl int) {
	C.gousb_set_debug((*C.libusb_context)(c), C.int(lvl))
}

func (libusbImpl) getDeviceDesc(d *libusbDevice) (*DeviceDesc, error) {
	var (
		desc     C.struct_libusb_device_descriptor
		pathData [8]uint8
		path     []int
		port     int
	)
	if err := fromErrNo(C.libusb_get_device_descriptor((*C.libusb_device)(d), &desc)); err != nil {
		return nil, err
	}
	pathLen := int(C.libusb_get_port_numbers((*C.libusb_device)(d), (*C.uint8_t)(&pathData[0]), 8))
	for _, nPort := range pathData[:pathLen] {
		port = int(nPort)
		path = append(path, port)
	}
	// Defaults to port = 0, path = [] for root device
	dev := &DeviceDesc{
		Bus:                  int(C.libusb_get_bus_number((*C.libusb_device)(d))),
		Address:              int(C.libusb_get_device_address((*C.libusb_device)(d))),
		Port:                 port,
		Path:                 path,
		Speed:                Speed(C.libusb_get_device_speed((*C.libusb_device)(d))),
		Spec:                 BCD(desc.bcdUSB),
		Device:               BCD(desc.bcdDevice),
		Vendor:               ID(desc.idVendor),
		Product:              ID(desc.idProduct),
		Class:                Class(desc.bDeviceClass),
		SubClass:             Class(desc.bDeviceSubClass),
		Protocol:             Protocol(desc.bDeviceProtocol),
		MaxControlPacketSize: int(desc.bMaxPacketSize0),
		iManufacturer:        int(desc.iManufacturer),
		iProduct:             int(desc.iProduct),
		iSerialNumber:        int(desc.iSerialNumber),
	}
	// Enumerate configurations
	cfgs := make(map[int]ConfigDesc)
	for i := 0; i < int(desc.bNumConfigurations); i++ {
		var cfg *C.struct_libusb_config_descriptor
		if err := fromErrNo(C.libusb_get_config_descriptor((*C.libusb_device)(d), C.uint8_t(i), &cfg)); err != nil {
			return nil, err
		}
		c := ConfigDesc{
			Number:         int(cfg.bConfigurationValue),
			SelfPowered:    (cfg.bmAttributes & selfPoweredMask) != 0,
			RemoteWakeup:   (cfg.bmAttributes & remoteWakeupMask) != 0,
			MaxPower:       2 * Milliamperes(cfg.MaxPower),
			iConfiguration: int(cfg.iConfiguration),
		}
		// at GenX speeds MaxPower is expressed in units of 8mA, not 2mA.
		if dev.Speed == SpeedSuper {
			c.MaxPower *= 4
		}

		var ifaces []C.struct_libusb_interface
		*(*reflect.SliceHeader)(unsafe.Pointer(&ifaces)) = reflect.SliceHeader{
			Data: uintptr(unsafe.Pointer(cfg._interface)),
			Len:  int(cfg.bNumInterfaces),
			Cap:  int(cfg.bNumInterfaces),
		}
		c.Interfaces = make([]InterfaceDesc, 0, len(ifaces))
		// a map of interface numbers to a set of alternate settings numbers
		hasIntf := make(map[int]map[int]bool)
		for _, iface := range ifaces {
			if iface.num_altsetting == 0 {
				continue
			}

			var alts []C.struct_libusb_interface_descriptor
			*(*reflect.SliceHeader)(unsafe.Pointer(&alts)) = reflect.SliceHeader{
				Data: uintptr(unsafe.Pointer(iface.altsetting)),
				Len:  int(iface.num_altsetting),
				Cap:  int(iface.num_altsetting),
			}
			descs := make([]InterfaceSetting, 0, len(alts))
			for _, alt := range alts {
				i := InterfaceSetting{
					Number:     int(alt.bInterfaceNumber),
					Alternate:  int(alt.bAlternateSetting),
					Class:      Class(alt.bInterfaceClass),
					SubClass:   Class(alt.bInterfaceSubClass),
					Protocol:   Protocol(alt.bInterfaceProtocol),
					iInterface: int(alt.iInterface),
				}

				if hasIntf[i.Number][i.Alternate] {
					log.Printf("Device on bus %d address %d offered a descriptor for config %d with two different entries with the same interface number (%d) and the same alternate setting number (%d). gousb will use only the first one.", dev.Bus, dev.Address, c.Number, i.Number, i.Alternate)
					continue
				}
				if hasIntf[i.Number] == nil {
					hasIntf[i.Number] = make(map[int]bool)
				}
				hasIntf[i.Number][i.Alternate] = true

				var ends []C.struct_libusb_endpoint_descriptor
				*(*reflect.SliceHeader)(unsafe.Pointer(&ends)) = reflect.SliceHeader{
					Data: uintptr(unsafe.Pointer(alt.endpoint)),
					Len:  int(alt.bNumEndpoints),
					Cap:  int(alt.bNumEndpoints),
				}
				i.Endpoints = make(map[EndpointAddress]EndpointDesc, len(ends))
				for _, end := range ends {
					epi := libusbEndpoint(end).endpointDesc(dev)
					i.Endpoints[epi.Address] = epi
				}
				descs = append(descs, i)
			}
			c.Interfaces = append(c.Interfaces, InterfaceDesc{
				Number:      descs[0].Number,
				AltSettings: descs,
			})
		}
		C.libusb_free_config_descriptor(cfg)
		cfgs[c.Number] = c
	}

	dev.Configs = cfgs
	return dev, nil
}

func (libusbImpl) dereference(d *libusbDevice) {
	C.libusb_unref_device((*C.libusb_device)(d))
}

func (libusbImpl) open(d *libusbDevice) (*libusbDevHandle, error) {
	var handle *C.libusb_device_handle
	if err := fromErrNo(C.libusb_open((*C.libusb_device)(d), &handle)); err != nil {
		return nil, err
	}
	return (*libusbDevHandle)(handle), nil
}

func (libusbImpl) close(d *libusbDevHandle) {
	C.libusb_close((*C.libusb_device_handle)(d))
}

func (libusbImpl) reset(d *libusbDevHandle) error {
	return fromErrNo(C.libusb_reset_device((*C.libusb_device_handle)(d)))
}

func (libusbImpl) control(d *libusbDevHandle, timeout time.Duration, rType, request uint8, val, idx uint16, data []byte) (int, error) {
	dataSlice := (*reflect.SliceHeader)(unsafe.Pointer(&data))
	n := C.libusb_control_transfer(
		(*C.libusb_device_handle)(d),
		C.uint8_t(rType),
		C.uint8_t(request),
		C.uint16_t(val),
		C.uint16_t(idx),
		(*C.uchar)(unsafe.Pointer(dataSlice.Data)),
		C.uint16_t(len(data)),
		C.uint(timeout/time.Millisecond))
	if n < 0 {
		return int(n), fromErrNo(n)
	}
	return int(n), nil
}

func (libusbImpl) bulk(d *libusbDevHandle, address EndpointAddress, data []byte, timeout time.Duration) (int, error) {
	dataSlice := (*reflect.SliceHeader)(unsafe.Pointer(&data))
	var cnt C.int
	if errno := C.libusb_bulk_transfer(
		(*C.libusb_device_handle)(d),
		C.uint8_t(address),
		(*C.uchar)(unsafe.Pointer(dataSlice.Data)),
		C.int(len(data)),
		&cnt,
		C.uint(timeout/time.Millisecond)); errno < 0 {
		return 0, fromErrNo(errno)
	}
	return int(cnt), nil
}

func (libusbImpl) getConfig(d *libusbDevHandle) (uint8, error) {
	var cfg C.int
	if errno := C.libusb_get_configuration((*C.libusb_device_handle)(d), &cfg); errno < 0 {
		return 0, fromErrNo(errno)
	}
	return uint8(cfg), nil
}

func (libusbImpl) setConfig(d *libusbDevHandle, cfg uint8) error {
	return fromErrNo(C.libusb_set_configuration((*C.libusb_device_handle)(d), C.int(cfg)))
}

// TODO(sebek): device string descriptors are natively in UTF16 and support
// multiple languages. get_string_descriptor_ascii uses always the first
// language and discards non-ascii bytes. We could do better if needed.
func (libusbImpl) getStringDesc(d *libusbDevHandle, index int) (string, error) {
	// allocate 200-byte array limited the length of string descriptor
	buf := make([]byte, 200)
	// get string descriptor from libusb. if errno < 0 then there are any errors.
	// if errno >= 0; it is a length of result string descriptor
	errno := C.libusb_get_string_descriptor_ascii(
		(*C.libusb_device_handle)(d),
		C.uint8_t(index),
		(*C.uchar)(unsafe.Pointer(&buf[0])),
		200)
	if errno < 0 {
		return "", fmt.Errorf("failed to get string descriptor %d: %s", index, fromErrNo(errno))
	}
	return string(buf[:errno]), nil
}

func (libusbImpl) setAutoDetach(d *libusbDevHandle, val int) error {
	err := fromErrNo(C.libusb_set_auto_detach_kernel_driver((*C.libusb_device_handle)(d), C.int(val)))
	if err != nil && err != ErrorNotSupported {
		return err
	}
	return nil
}

func (libusbImpl) detachKernelDriver(d *libusbDevHandle, iface uint8) error {
	err := fromErrNo(C.libusb_detach_kernel_driver((*C.libusb_device_handle)(d), C.int(iface)))
	if err != nil && err != ErrorNotSupported && err != ErrorNotFound {
		// ErrorNotSupported is returned in non linux systems
		// ErrorNotFound is returned if libusb's driver is already attached to the device
		return err
	}
	return nil
}

func (libusbImpl) claim(d *libusbDevHandle, iface uint8) error {
	return fromErrNo(C.libusb_claim_interface((*C.libusb_device_handle)(d), C.int(iface)))
}

func (libusbImpl) release(d *libusbDevHandle, iface uint8) {
	C.libusb_release_interface((*C.libusb_device_handle)(d), C.int(iface))
}

func (libusbImpl) setAlt(d *libusbDevHandle, iface, setup uint8) error {
	return fromErrNo(C.libusb_set_interface_alt_setting((*C.libusb_device_handle)(d), C.int(iface), C.int(setup)))
}

func (libusbImpl) alloc(d *libusbDevHandle, ep *EndpointDesc, isoPackets int, bufLen int, done chan struct{}) (*libusbTransfer, error) {
	xfer := C.gousb_alloc_transfer_and_buffer(C.int(bufLen), C.int(isoPackets))
	if xfer == nil {
		return nil, fmt.Errorf("gousb_alloc_transfer_and_buffer(%d, %d) failed", bufLen, isoPackets)
	}
	if int(xfer.length) != bufLen {
		return nil, fmt.Errorf("gousb_alloc_transfer_and_buffer(%d, %d): length = %d, want %d", bufLen, isoPackets, xfer.length, bufLen)
	}
	xfer.dev_handle = (*C.libusb_device_handle)(d)
	xfer.endpoint = C.uchar(ep.Address)
	xfer._type = C.uchar(ep.TransferType)
	xfer.num_iso_packets = C.int(isoPackets)
	ret := (*libusbTransfer)(xfer)
	xferDoneMap.Lock()
	xferDoneMap.m[ret] = done
	xferDoneMap.Unlock()
	return ret, nil
}

func (libusbImpl) cancel(t *libusbTransfer) error {
	return fromErrNo(C.libusb_cancel_transfer((*C.struct_libusb_transfer)(t)))
}

func (libusbImpl) submit(t *libusbTransfer) error {
	return fromErrNo(C.submit((*C.struct_libusb_transfer)(t)))
}

func (libusbImpl) buffer(t *libusbTransfer) []byte {
	// TODO(go1.10?): replace with more user-friendly construct once
	// one exists. https://github.com/golang/go/issues/13656
	var ret []byte
	*(*reflect.SliceHeader)(unsafe.Pointer(&ret)) = reflect.SliceHeader{
		Data: uintptr(unsafe.Pointer(t.buffer)),
		Len:  int(t.length),
		Cap:  int(t.length),
	}
	return ret
}

func (libusbImpl) data(t *libusbTransfer) (int, TransferStatus) {
	if TransferType(t._type) == TransferTypeIsochronous {
		var status TransferStatus
		n := int(C.gousb_compact_iso_data((*C.struct_libusb_transfer)(t), (*C.uchar)(unsafe.Pointer(&status))))
		return n, status
	}
	return int(t.actual_length), TransferStatus(t.status)
}

func (libusbImpl) free(t *libusbTransfer) {
	xferDoneMap.Lock()
	delete(xferDoneMap.m, t)
	xferDoneMap.Unlock()
	C.gousb_free_transfer_and_buffer((*C.struct_libusb_transfer)(t))
}

func (libusbImpl) setIsoPacketLengths(t *libusbTransfer, length uint32) {
	C.libusb_set_iso_packet_lengths((*C.struct_libusb_transfer)(t), C.uint(length))
}

// xferDoneMap keeps a map of done callback channels for all allocated transfers.
var xferDoneMap = struct {
	m map[*libusbTransfer]chan struct{}
	sync.RWMutex
}{
	m: make(map[*libusbTransfer]chan struct{}),
}

//export xferCallback
func xferCallback(xfer *C.struct_libusb_transfer) {
	xferDoneMap.RLock()
	ch := xferDoneMap.m[(*libusbTransfer)(xfer)]
	xferDoneMap.RUnlock()
	ch <- struct{}{}
}

// for benchmarking of method on implementation vs vanilla function.
func libusbSetDebug(c *libusbContext, lvl int) {
	C.gousb_set_debug((*C.libusb_context)(c), C.int(lvl))
}

// for obtaining unique CGo pointers.
func newDevicePointer() *libusbDevice {
	return (*libusbDevice)(unsafe.Pointer(C.malloc(1)))
}

func newFakeTransferPointer() *libusbTransfer {
	return (*libusbTransfer)(unsafe.Pointer(C.malloc(1)))
}

func newContextPointer() *libusbContext {
	return (*libusbContext)(unsafe.Pointer(C.malloc(1)))
}

func newDevHandlePointer() *libusbDevHandle {
	return (*libusbDevHandle)(unsafe.Pointer(C.malloc(1)))
}

// hotplugCallbackMap keeps a map of go callback functions for libusb hotplug callbacks
// for each context.
// When a context is closed with libusb_exit, its callbacks are automatically deregistered
// by libusb, and they are removed from this map too.
var hotplugCallbackMap = struct {
	m map[*libusbContext]map[unsafe.Pointer]libusbHotplugCallback
	sync.RWMutex
}{
	m: make(map[*libusbContext]map[unsafe.Pointer]libusbHotplugCallback),
}

//export goHotplugCallback
func goHotplugCallback(ctx *C.libusb_context, device *C.libusb_device, event C.libusb_hotplug_event, userData unsafe.Pointer) C.int {
	var fn libusbHotplugCallback
	hotplugCallbackMap.RLock()
	m, ok := hotplugCallbackMap.m[(*libusbContext)(ctx)]
	if ok {
		fn, ok = m[userData]
	}
	hotplugCallbackMap.RUnlock()
	if !ok {
		// This shouldn't happen. Deregister the callback.
		return 1
	}
	dereg := fn((*libusbContext)(ctx), (*libusbDevice)(device), HotplugEventType(event))

	if dereg {
		hotplugCallbackMap.Lock()
		m, ok := hotplugCallbackMap.m[(*libusbContext)(ctx)]
		if ok {
			delete(m, userData)
			C.free(userData)
		}
		hotplugCallbackMap.Unlock()
		return 1
	}
	return 0
}

func (libusbImpl) registerHotplugCallback(ctx *libusbContext, events HotplugEventType, enumerate bool, vendorID int32, productID int32, devClass int32, fn libusbHotplugCallback) (func(), error) {
	// We must allocate memory to pass to C, since we can't pass a go pointer.
	// We can use the resulting pointer as a map key instead of
	// storing the map key inside the memory allocated.
	id := C.malloc(1)
	if id == nil {
		panic(errors.New("failed to allocate memory during callback registration"))
	}
	hotplugCallbackMap.Lock()
	m, ok := hotplugCallbackMap.m[ctx]
	if !ok {
		hotplugCallbackMap.m[ctx] = make(map[unsafe.Pointer]libusbHotplugCallback)
		m = hotplugCallbackMap.m[ctx]
	}
	m[id] = fn
	hotplugCallbackMap.Unlock()

	var flags C.libusb_hotplug_flag
	if enumerate {
		flags = C.LIBUSB_HOTPLUG_ENUMERATE
	}
	var handle C.libusb_hotplug_callback_handle

	// TODO: figure out how to run deregister in callback.
	// There's a race condition here, because the callback may be called before
	// gousb_hotplug_register_callback returns, depending on libusb's implementation.
	res := C.gousb_hotplug_register_callback((*C.libusb_context)(ctx), C.libusb_hotplug_event(events), flags, C.int(vendorID), C.int(productID), C.int(devClass), id, &handle)
	err := fromErrNo(res)
	if err != nil {
		hotplugCallbackMap.Lock()
		delete(hotplugCallbackMap.m[ctx], id)
		hotplugCallbackMap.Unlock()
		C.free(id)
		return nil, err
	}

	return func() {
		C.libusb_hotplug_deregister_callback((*C.libusb_context)(ctx), handle)
		hotplugCallbackMap.Lock()
		m, ok := hotplugCallbackMap.m[ctx]
		if ok {
			delete(m, id)
			C.free(id)
		}
		hotplugCallbackMap.Unlock()
	}, nil
}
