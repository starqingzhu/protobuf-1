// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package impl

import (
	"fmt"
	"math"
	"math/bits"
	"reflect"
	"unicode/utf8"

	"google.golang.org/protobuf/internal/encoding/wire"
	"google.golang.org/protobuf/internal/strs"
	pref "google.golang.org/protobuf/reflect/protoreflect"
	preg "google.golang.org/protobuf/reflect/protoregistry"
	piface "google.golang.org/protobuf/runtime/protoiface"
)

// ValidationStatus is the result of validating the wire-format encoding of a message.
type ValidationStatus int

const (
	// ValidationUnknown indicates that unmarshaling the message might succeed or fail.
	// The validator was unable to render a judgement.
	//
	// The only causes of this status are an aberrant message type appearing somewhere
	// in the message or a failure in the extension resolver.
	ValidationUnknown ValidationStatus = iota + 1

	// ValidationInvalid indicates that unmarshaling the message will fail.
	ValidationInvalid

	// ValidationValidInitialized indicates that unmarshaling the message will succeed
	// and IsInitialized on the result will report success.
	ValidationValidInitialized

	// ValidationValidMaybeUninitalized indicates unmarshaling the message will succeed,
	// but the output of IsInitialized on the result is unknown.
	//
	// This status may be returned for an initialized message when a message value
	// is split across multiple fields.
	ValidationValidMaybeUninitalized
)

func (v ValidationStatus) String() string {
	switch v {
	case ValidationUnknown:
		return "ValidationUnknown"
	case ValidationInvalid:
		return "ValidationInvalid"
	case ValidationValidInitialized:
		return "ValidationValidInitialized"
	case ValidationValidMaybeUninitalized:
		return "ValidationValidMaybeUninitalized"
	default:
		return fmt.Sprintf("ValidationStatus(%d)", int(v))
	}
}

// Validate determines whether the contents of the buffer are a valid wire encoding
// of the message type.
//
// This function is exposed for testing.
func Validate(b []byte, mt pref.MessageType, opts piface.UnmarshalOptions) ValidationStatus {
	mi, ok := mt.(*MessageInfo)
	if !ok {
		return ValidationUnknown
	}
	return mi.validate(b, 0, newUnmarshalOptions(opts))
}

type validationInfo struct {
	mi               *MessageInfo
	typ              validationType
	keyType, valType validationType

	// For non-required fields, requiredIndex is 0.
	//
	// For required fields, requiredIndex is unique index in the range
	// (0, MessageInfo.numRequiredFields].
	requiredIndex uint8
}

type validationType uint8

const (
	validationTypeOther validationType = iota
	validationTypeMessage
	validationTypeGroup
	validationTypeMap
	validationTypeRepeatedVarint
	validationTypeRepeatedFixed32
	validationTypeRepeatedFixed64
	validationTypeVarint
	validationTypeFixed32
	validationTypeFixed64
	validationTypeBytes
	validationTypeUTF8String
)

func newFieldValidationInfo(mi *MessageInfo, si structInfo, fd pref.FieldDescriptor, ft reflect.Type) validationInfo {
	var vi validationInfo
	switch {
	case fd.ContainingOneof() != nil:
		switch fd.Kind() {
		case pref.MessageKind:
			vi.typ = validationTypeMessage
			if ot, ok := si.oneofWrappersByNumber[fd.Number()]; ok {
				vi.mi = getMessageInfo(ot.Field(0).Type)
			}
		case pref.GroupKind:
			vi.typ = validationTypeGroup
			if ot, ok := si.oneofWrappersByNumber[fd.Number()]; ok {
				vi.mi = getMessageInfo(ot.Field(0).Type)
			}
		case pref.StringKind:
			if strs.EnforceUTF8(fd) {
				vi.typ = validationTypeUTF8String
			}
		}
	default:
		vi = newValidationInfo(fd, ft)
	}
	if fd.Cardinality() == pref.Required {
		// Avoid overflow. The required field check is done with a 64-bit mask, with
		// any message containing more than 64 required fields always reported as
		// potentially uninitialized, so it is not important to get a precise count
		// of the required fields past 64.
		if mi.numRequiredFields < math.MaxUint8 {
			mi.numRequiredFields++
			vi.requiredIndex = mi.numRequiredFields
		}
	}
	return vi
}

func newValidationInfo(fd pref.FieldDescriptor, ft reflect.Type) validationInfo {
	var vi validationInfo
	switch {
	case fd.IsList():
		switch fd.Kind() {
		case pref.MessageKind:
			vi.typ = validationTypeMessage
			if ft.Kind() == reflect.Slice {
				vi.mi = getMessageInfo(ft.Elem())
			}
		case pref.GroupKind:
			vi.typ = validationTypeGroup
			if ft.Kind() == reflect.Slice {
				vi.mi = getMessageInfo(ft.Elem())
			}
		case pref.StringKind:
			vi.typ = validationTypeBytes
			if strs.EnforceUTF8(fd) {
				vi.typ = validationTypeUTF8String
			}
		default:
			switch wireTypes[fd.Kind()] {
			case wire.VarintType:
				vi.typ = validationTypeRepeatedVarint
			case wire.Fixed32Type:
				vi.typ = validationTypeRepeatedFixed32
			case wire.Fixed64Type:
				vi.typ = validationTypeRepeatedFixed64
			}
		}
	case fd.IsMap():
		vi.typ = validationTypeMap
		switch fd.MapKey().Kind() {
		case pref.StringKind:
			if strs.EnforceUTF8(fd) {
				vi.keyType = validationTypeUTF8String
			}
		}
		switch fd.MapValue().Kind() {
		case pref.MessageKind:
			vi.valType = validationTypeMessage
			if ft.Kind() == reflect.Map {
				vi.mi = getMessageInfo(ft.Elem())
			}
		case pref.StringKind:
			if strs.EnforceUTF8(fd) {
				vi.valType = validationTypeUTF8String
			}
		}
	default:
		switch fd.Kind() {
		case pref.MessageKind:
			vi.typ = validationTypeMessage
			if !fd.IsWeak() {
				vi.mi = getMessageInfo(ft)
			}
		case pref.GroupKind:
			vi.typ = validationTypeGroup
			vi.mi = getMessageInfo(ft)
		case pref.StringKind:
			vi.typ = validationTypeBytes
			if strs.EnforceUTF8(fd) {
				vi.typ = validationTypeUTF8String
			}
		default:
			switch wireTypes[fd.Kind()] {
			case wire.VarintType:
				vi.typ = validationTypeVarint
			case wire.Fixed32Type:
				vi.typ = validationTypeFixed32
			case wire.Fixed64Type:
				vi.typ = validationTypeFixed64
			}
		}
	}
	return vi
}

func (mi *MessageInfo) validate(b []byte, groupTag wire.Number, opts unmarshalOptions) (result ValidationStatus) {
	type validationState struct {
		typ              validationType
		keyType, valType validationType
		endGroup         wire.Number
		mi               *MessageInfo
		tail             []byte
		requiredMask     uint64
	}

	// Pre-allocate some slots to avoid repeated slice reallocation.
	states := make([]validationState, 0, 16)
	states = append(states, validationState{
		typ: validationTypeMessage,
		mi:  mi,
	})
	if groupTag > 0 {
		states[0].typ = validationTypeGroup
		states[0].endGroup = groupTag
	}
	initialized := true
State:
	for len(states) > 0 {
		st := &states[len(states)-1]
		if st.mi != nil {
			st.mi.init()
		}
	Field:
		for len(b) > 0 {
			num, wtyp, n := wire.ConsumeTag(b)
			if n < 0 {
				return ValidationInvalid
			}
			b = b[n:]
			if num > wire.MaxValidNumber {
				return ValidationInvalid
			}
			if wtyp == wire.EndGroupType {
				if st.endGroup == num {
					goto PopState
				}
				return ValidationInvalid
			}
			var vi validationInfo
			switch st.typ {
			case validationTypeMap:
				switch num {
				case 1:
					vi.typ = st.keyType
				case 2:
					vi.typ = st.valType
					vi.mi = st.mi
				}
			default:
				var f *coderFieldInfo
				if int(num) < len(st.mi.denseCoderFields) {
					f = st.mi.denseCoderFields[num]
				} else {
					f = st.mi.coderFields[num]
				}
				if f != nil {
					vi = f.validation
					if vi.typ == validationTypeMessage && vi.mi == nil {
						// Probable weak field.
						//
						// TODO: Consider storing the results of this lookup somewhere
						// rather than recomputing it on every validation.
						fd := st.mi.Desc.Fields().ByNumber(num)
						if fd == nil || !fd.IsWeak() {
							break
						}
						messageName := fd.Message().FullName()
						messageType, err := preg.GlobalTypes.FindMessageByName(messageName)
						switch err {
						case nil:
							vi.mi, _ = messageType.(*MessageInfo)
						case preg.NotFound:
							vi.typ = validationTypeBytes
						default:
							return ValidationUnknown
						}
					}
					break
				}
				// Possible extension field.
				//
				// TODO: We should return ValidationUnknown when:
				//   1. The resolver is not frozen. (More extensions may be added to it.)
				//   2. The resolver returns preg.NotFound.
				// In this case, a type added to the resolver in the future could cause
				// unmarshaling to begin failing. Supporting this requires some way to
				// determine if the resolver is frozen.
				xt, err := opts.Resolver().FindExtensionByNumber(st.mi.Desc.FullName(), num)
				if err != nil && err != preg.NotFound {
					return ValidationUnknown
				}
				if err == nil {
					vi = getExtensionFieldInfo(xt).validation
				}
			}
			if vi.requiredIndex > 0 {
				// Check that the field has a compatible wire type.
				// We only need to consider non-repeated field types,
				// since repeated fields (and maps) can never be required.
				ok := false
				switch vi.typ {
				case validationTypeVarint:
					ok = wtyp == wire.VarintType
				case validationTypeFixed32:
					ok = wtyp == wire.Fixed32Type
				case validationTypeFixed64:
					ok = wtyp == wire.Fixed64Type
				case validationTypeBytes, validationTypeUTF8String, validationTypeMessage, validationTypeGroup:
					ok = wtyp == wire.BytesType
				}
				if ok {
					st.requiredMask |= 1 << (vi.requiredIndex - 1)
				}
			}
			switch vi.typ {
			case validationTypeMessage, validationTypeMap:
				if wtyp != wire.BytesType {
					break
				}
				if vi.mi == nil && vi.typ == validationTypeMessage {
					return ValidationUnknown
				}
				size, n := wire.ConsumeVarint(b)
				if n < 0 {
					return ValidationInvalid
				}
				b = b[n:]
				if uint64(len(b)) < size {
					return ValidationInvalid
				}
				states = append(states, validationState{
					typ:     vi.typ,
					keyType: vi.keyType,
					valType: vi.valType,
					mi:      vi.mi,
					tail:    b[size:],
				})
				b = b[:size]
				continue State
			case validationTypeGroup:
				if wtyp != wire.StartGroupType {
					break
				}
				if vi.mi == nil {
					return ValidationUnknown
				}
				states = append(states, validationState{
					typ:      validationTypeGroup,
					mi:       vi.mi,
					endGroup: num,
				})
				continue State
			case validationTypeRepeatedVarint:
				if wtyp != wire.BytesType {
					break
				}
				// Packed field.
				v, n := wire.ConsumeBytes(b)
				if n < 0 {
					return ValidationInvalid
				}
				b = b[n:]
				for len(v) > 0 {
					_, n := wire.ConsumeVarint(v)
					if n < 0 {
						return ValidationInvalid
					}
					v = v[n:]
				}
				continue Field
			case validationTypeRepeatedFixed32:
				if wtyp != wire.BytesType {
					break
				}
				// Packed field.
				v, n := wire.ConsumeBytes(b)
				if n < 0 || len(v)%4 != 0 {
					return ValidationInvalid
				}
				b = b[n:]
				continue Field
			case validationTypeRepeatedFixed64:
				if wtyp != wire.BytesType {
					break
				}
				// Packed field.
				v, n := wire.ConsumeBytes(b)
				if n < 0 || len(v)%8 != 0 {
					return ValidationInvalid
				}
				b = b[n:]
				continue Field
			case validationTypeUTF8String:
				if wtyp != wire.BytesType {
					break
				}
				v, n := wire.ConsumeBytes(b)
				if n < 0 || !utf8.Valid(v) {
					return ValidationInvalid
				}
				b = b[n:]
				continue Field
			}
			n = wire.ConsumeFieldValue(num, wtyp, b)
			if n < 0 {
				return ValidationInvalid
			}
			b = b[n:]
		}
		if st.endGroup != 0 {
			return ValidationInvalid
		}
		if len(b) != 0 {
			return ValidationInvalid
		}
		b = st.tail
	PopState:
		switch st.typ {
		case validationTypeMessage, validationTypeGroup:
			// If there are more than 64 required fields, this check will
			// always fail and we will report that the message is potentially
			// uninitialized.
			if st.mi.numRequiredFields > 0 && bits.OnesCount64(st.requiredMask) != int(st.mi.numRequiredFields) {
				initialized = false
			}
		}
		states = states[:len(states)-1]
	}
	if !initialized {
		return ValidationValidMaybeUninitalized
	}
	return ValidationValidInitialized
}
