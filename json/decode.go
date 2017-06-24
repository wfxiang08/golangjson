// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Represents JSON data structure using native Go types: booleans, floats,
// strings, arrays, and maps.

package json

import (
	"bytes"
	"encoding"
	"encoding/base64"
	"errors"
	"fmt"
	log "github.com/wfxiang08/cyutils/utils/rolling_log"
	"reflect"
	"runtime"
	"strconv"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Unmarshal parses the JSON-encoded data and stores the result
// in the value pointed to by v.
//
// Unmarshal uses the inverse of the encodings that
// Marshal uses, allocating maps, slices, and pointers as necessary,
// with the following additional rules:
//
// To unmarshal JSON into a pointer, Unmarshal first handles the case of
// the JSON being the JSON literal null. In that case, Unmarshal sets
// the pointer to nil. Otherwise, Unmarshal unmarshals the JSON into
// the value pointed at by the pointer. If the pointer is nil, Unmarshal
// allocates a new value for it to point to.
//
// To unmarshal JSON into a value implementing the Unmarshaler interface,
// Unmarshal calls that value's UnmarshalJSON method, including
// when the input is a JSON null.
// Otherwise, if the value implements encoding.TextUnmarshaler
// and the input is a JSON quoted string, Unmarshal calls that value's
// UnmarshalText method with the unquoted form of the string.
//
// To unmarshal JSON into a struct, Unmarshal matches incoming object
// keys to the keys used by Marshal (either the struct field name or its tag),
// preferring an exact match but also accepting a case-insensitive match.
// Unmarshal will only set exported fields of the struct.
//
// To unmarshal JSON into an interface value,
// Unmarshal stores one of these in the interface value:
//
//	bool, for JSON booleans
//	float64, for JSON numbers
//	string, for JSON strings
//	[]interface{}, for JSON arrays
//	map[string]interface{}, for JSON objects
//	nil for JSON null
//
// To unmarshal a JSON array into a slice, Unmarshal resets the slice length
// to zero and then appends each element to the slice.
// As a special case, to unmarshal an empty JSON array into a slice,
// Unmarshal replaces the slice with a new empty slice.
//
// To unmarshal a JSON array into a Go array, Unmarshal decodes
// JSON array elements into corresponding Go array elements.
// If the Go array is smaller than the JSON array,
// the additional JSON array elements are discarded.
// If the JSON array is smaller than the Go array,
// the additional Go array elements are set to zero values.
//
// To unmarshal a JSON object into a map, Unmarshal first establishes a map to
// use. If the map is nil, Unmarshal allocates a new map. Otherwise Unmarshal
// reuses the existing map, keeping existing entries. Unmarshal then stores
// key-value pairs from the JSON object into the map. The map's key type must
// either be a string, an integer, or implement encoding.TextUnmarshaler.
//
// If a JSON value is not appropriate for a given target type,
// or if a JSON number overflows the target type, Unmarshal
// skips that field and completes the unmarshaling as best it can.
// If no more serious errors are encountered, Unmarshal returns
// an UnmarshalTypeError describing the earliest such error.
//
// The JSON null value unmarshals into an interface, map, pointer, or slice
// by setting that Go value to nil. Because null is often used in JSON to mean
// ``not present,'' unmarshaling a JSON null into any other Go type has no effect
// on the value and produces no error.
//
// When unmarshaling quoted strings, invalid UTF-8 or
// invalid UTF-16 surrogate pairs are not treated as an error.
// Instead, they are replaced by the Unicode replacement
// character U+FFFD.
//

func stack() []byte {
	buf := make([]byte, 1024)
	n := runtime.Stack(buf, false)
	return buf[:n]
}
func printStack() {
	log.Info(fmt.Sprintf("%s", stack()))
	log.Info("end here.")
}

// 如何解析json呢?
func Unmarshal(data []byte, v interface{}) error {
	// Check for well-formedness.
	// Avoids filling out half a data structure
	// before discovering a JSON syntax error.
	var d decodeState

	// 首先检查json是否有效?
	err := checkValid(data, &d.scan)
	if err != nil {
		log.ErrorErrorf(err, "CheckValid failed")
		return err
	}

	// 初始化状态
	d.init(data)

	// 开始unmarshal
	return d.unmarshal(v)
}

// Unmarshaler is the interface implemented by types
// that can unmarshal a JSON description of themselves.
// The input can be assumed to be a valid encoding of
// a JSON value. UnmarshalJSON must copy the JSON data
// if it wishes to retain the data after returning.
//
// By convention, to approximate the behavior of Unmarshal itself,
// Unmarshalers implement UnmarshalJSON([]byte("null")) as a no-op.
type Unmarshaler interface {
	UnmarshalJSON([]byte) error
}

// An UnmarshalTypeError describes a JSON value that was
// not appropriate for a value of a specific Go type.
type UnmarshalTypeError struct {
	Value  string       // description of JSON value - "bool", "array", "number -5"
	Type   reflect.Type // type of Go value it could not be assigned to
	Offset int64        // error occurred after reading Offset bytes
	Struct string       // name of the struct type containing the field
	Field  string       // name of the field holding the Go value
}

func (e *UnmarshalTypeError) Error() string {
	if e.Struct != "" || e.Field != "" {
		return "json: cannot unmarshal " + e.Value + " into Go struct field " + e.Struct + "." + e.Field + " of type " + e.Type.String()
	}
	return "json: cannot unmarshal " + e.Value + " into Go value of type " + e.Type.String()
}

// An UnmarshalFieldError describes a JSON object key that
// led to an unexported (and therefore unwritable) struct field.
// (No longer used; kept for compatibility.)
type UnmarshalFieldError struct {
	Key   string
	Type  reflect.Type
	Field reflect.StructField
}

func (e *UnmarshalFieldError) Error() string {
	return "json: cannot unmarshal object key " + strconv.Quote(e.Key) + " into unexported field " + e.Field.Name + " of type " + e.Type.String()
}

// An InvalidUnmarshalError describes an invalid argument passed to Unmarshal.
// (The argument to Unmarshal must be a non-nil pointer.)
type InvalidUnmarshalError struct {
	Type reflect.Type
}

func (e *InvalidUnmarshalError) Error() string {
	if e.Type == nil {
		return "json: Unmarshal(nil)"
	}

	if e.Type.Kind() != reflect.Ptr {
		return "json: Unmarshal(non-pointer " + e.Type.String() + ")"
	}
	return "json: Unmarshal(nil " + e.Type.String() + ")"
}

func (d *decodeState) unmarshal(v interface{}) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(runtime.Error); ok {
				log.ErrorErrorf(e, "unmarshal error")
				panic(r)
			}
			err = r.(error)
		}
	}()

	// interface{} --> reflect.Value
	rv := reflect.ValueOf(v)

	// 必须使用Ptr, 非空
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return &InvalidUnmarshalError{reflect.TypeOf(v)}
	}

	// 开始
	d.scan.reset()

	// We decode rv not rv.Elem because the Unmarshaler interface
	// test must be applied at the top level of the value.
	d.value(rv)
	return d.savedError
}

//
// http://www.json.org/json-zh.html
// JSON Number的格式
//
func isValidNumber(s string) bool {
	// This function implements the JSON numbers grammar.
	// See https://tools.ietf.org/html/rfc7159#section-6
	// and http://json.org/number.gif

	if s == "" {
		return false
	}

	// Optional -
	if s[0] == '-' {
		s = s[1:]
		if s == "" {
			return false
		}
	}

	// Digits
	switch {
	default:
		// 其他情况，失败
		return false
	// 一个0
	case s[0] == '0':
		s = s[1:]

	// 1-9, 然后多个数字
	case '1' <= s[0] && s[0] <= '9':
		s = s[1:]
		for len(s) > 0 && '0' <= s[0] && s[0] <= '9' {
			s = s[1:]
		}
	}

	// . followed by 1 or more digits.
	if len(s) >= 2 && s[0] == '.' && '0' <= s[1] && s[1] <= '9' {
		s = s[2:]
		for len(s) > 0 && '0' <= s[0] && s[0] <= '9' {
			s = s[1:]
		}
	}

	// e or E followed by an optional - or + and
	// 1 or more digits.
	if len(s) >= 2 && (s[0] == 'e' || s[0] == 'E') {
		s = s[1:]
		if s[0] == '+' || s[0] == '-' {
			s = s[1:]
			// +/-之后也需要有数字，太严格了
			if s == "" {
				return false
			}
		}
		for len(s) > 0 && '0' <= s[0] && s[0] <= '9' {
			s = s[1:]
		}
	}

	// Make sure we are at the end.
	return s == ""
}

// decodeState represents the state while decoding a JSON value.
type decodeState struct {
	data         []byte
	off          int     // read offset in data
	scan         scanner
	nextscan     scanner // for calls to nextValue
	errorContext struct {
		             // provides context for type errors
		             Struct string
		             Field  string
	             }
	savedError   error
	useNumber    bool
}

// errPhase is used for errors that should not happen unless
// there is a bug in the JSON decoder or something is editing
// the data slice while the decoder executes.
var errPhase = errors.New("JSON decoder out of sync - data changing underfoot?")

// 初始化状态
func (d *decodeState) init(data []byte) *decodeState {
	d.data = data
	d.off = 0
	d.savedError = nil
	d.errorContext.Struct = ""
	d.errorContext.Field = ""
	return d
}

// error aborts the decoding by panicking with err.
func (d *decodeState) error(err error) {
	log.ErrorErrorf(err, "==> Fatal error, panic")
	panic(d.addErrorContext(err))
}

// saveError saves the first err it is called with,
// for reporting at the end of the unmarshal.
func (d *decodeState) saveError(err error) {
	log.ErrorErrorf(err, "decodeState#saveError")
	if d.savedError == nil {
		d.savedError = d.addErrorContext(err)
	}
}

// addErrorContext returns a new error enhanced with information from d.errorContext
func (d *decodeState) addErrorContext(err error) error {
	if d.errorContext.Struct != "" || d.errorContext.Field != "" {
		switch err := err.(type) {
		case *UnmarshalTypeError:
			// 给Error添加Context
			err.Struct = d.errorContext.Struct
			err.Field = d.errorContext.Field
			return err
		}
	}
	return err
}

// next cuts off and returns the next full JSON value in d.data[d.off:].
// The next value is known to be an object or array, not a literal.
func (d *decodeState) next() []byte {
	c := d.data[d.off]

	item, rest, err := nextValue(d.data[d.off:], &d.nextscan)
	if err != nil {
		d.error(err)
	}
	d.off = len(d.data) - len(rest)

	// Our scanner has seen the opening brace/bracket
	// and thinks we're still in the middle of the object.
	// invent a closing brace/bracket to get it out.
	//
	if c == '{' {
		d.scan.step(&d.scan, '}')
	} else {
		d.scan.step(&d.scan, ']')
	}

	return item
}

// scanWhile processes bytes in d.data[d.off:] until it
// receives a scan code not equal to op.
// It updates d.off and returns the new scan code.
func (d *decodeState) scanWhile(op int) int {
	var newOp int
	for {
		if d.off >= len(d.data) {
			newOp = d.scan.eof()
			d.off = len(d.data) + 1 // mark processed EOF with len+1
		} else {
			c := d.data[d.off]
			d.off++
			newOp = d.scan.step(&d.scan, c)
		}
		if newOp != op {
			break
		}
	}
	return newOp
}

// value decodes a JSON value from d.data[d.off:] into the value.
// it updates d.off to point past the decoded value.
//
func (d *decodeState) value(v reflect.Value) {
	// 首先假定v是有效的？
	if !v.IsValid() {
		_, rest, err := nextValue(d.data[d.off:], &d.nextscan)
		if err != nil {
			d.error(err)
		}
		d.off = len(d.data) - len(rest)

		// d.scan thinks we're still at the beginning of the item.
		// Feed in an empty string - the shortest, simplest value -
		// so that it knows we got to the end of the value.
		if d.scan.redo {
			// rewind.
			d.scan.redo = false
			d.scan.step = stateBeginValue
		}
		d.scan.step(&d.scan, '"')
		d.scan.step(&d.scan, '"')

		n := len(d.scan.parseState)
		if n > 0 && d.scan.parseState[n - 1] == parseObjectKey {
			// d.scan thinks we just read an object key; finish the object
			d.scan.step(&d.scan, ':')
			d.scan.step(&d.scan, '"')
			d.scan.step(&d.scan, '"')
			d.scan.step(&d.scan, '}')
		}

		return
	}

	// 如何解码呢? 解码数据时都是literal, 没有variable
	// 遇到对象
	// 遇到数组
	// 遇到常数
	switch op := d.scanWhile(scanSkipSpace); op {
	default:
		d.error(errPhase)

	case scanBeginArray:
		d.array(v)

	case scanBeginObject:
		d.object(v)

	case scanBeginLiteral:
		d.literal(v)
	}
}

// 空对象
type unquotedValue struct{}

// valueQuoted is like value but decodes a
// quoted string literal or literal null into an interface value.
// If it finds anything other than a quoted string literal or null,
// valueQuoted returns unquotedValue{}.
func (d *decodeState) valueQuoted() interface{} {

	switch op := d.scanWhile(scanSkipSpace); op {
	default:
		d.error(errPhase)

	case scanBeginArray:
		d.array(reflect.Value{})

	case scanBeginObject:
		d.object(reflect.Value{})

	case scanBeginLiteral:
		return d.literalInterface()
	//switch v := d.literalInterface().(type) {
	//case nil, string:
	//	return v
	//}
	}
	return unquotedValue{}
}

// indirect walks down v allocating pointers as needed,
// until it gets to a non-pointer.
// if it encounters an Unmarshaler, indirect stops and returns that.
// if decodingNull is true, indirect stops at the last pointer so it can be set to nil.
func (d *decodeState) indirect(v reflect.Value, decodingNull bool) (Unmarshaler, encoding.TextUnmarshaler, reflect.Value) {
	// v是需要被修改的对象
	// 原始的json的数据在decodeState中
	//
	// If v is a named type and is addressable,
	// start with its address, so that if the type has pointer methods,
	// we find them.
	if v.Kind() != reflect.Ptr && v.Type().Name() != "" && v.CanAddr() {
		v = v.Addr()
	}
	for {
		// Load value from interface, but only if the result will be
		// usefully addressable.
		// interface必须被初始化，否则无法生成对象
		if v.Kind() == reflect.Interface && !v.IsNil() {
			e := v.Elem()
			if e.Kind() == reflect.Ptr && !e.IsNil() && (!decodingNull || e.Elem().Kind() == reflect.Ptr) {
				v = e
				continue
			}
		}

		if v.Kind() != reflect.Ptr {
			break
		}

		if v.Elem().Kind() != reflect.Ptr && decodingNull && v.CanSet() {
			break
		}

		// 创建一个v.Type()对象
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		if v.Type().NumMethod() > 0 {
			// 是否有自己的Unmarsher实现? 如果有则返回，如果没有则返回自身
			if u, ok := v.Interface().(Unmarshaler); ok {
				return u, nil, reflect.Value{}
			}
			if !decodingNull {
				if u, ok := v.Interface().(encoding.TextUnmarshaler); ok {
					return nil, u, reflect.Value{}
				}
			}
		}
		v = v.Elem()
	}
	return nil, nil, v
}

// array consumes an array from d.data[d.off-1:], decoding into the value v.
// the first byte of the array ('[') has been read already.
func (d *decodeState) array(v reflect.Value) {
	// Check for unmarshaler.
	u, ut, pv := d.indirect(v, false)

	// UnmarshalJSON
	if u != nil {
		d.off--
		err := u.UnmarshalJSON(d.next())
		if err != nil {
			d.error(err)
		}
		return
	}

	// ut
	if ut != nil {
		d.saveError(&UnmarshalTypeError{Value: "array", Type: v.Type(), Offset: int64(d.off)})
		d.off--
		d.next()
		return
	}

	v = pv

	// Check type of target.
	switch v.Kind() {
	case reflect.Interface:
		if v.NumMethod() == 0 {
			// Decoding into nil interface?  Switch to non-reflect code.
			v.Set(reflect.ValueOf(d.arrayInterface()))
			return
		}
		// Otherwise it's invalid.
		fallthrough
	default:
		d.saveError(&UnmarshalTypeError{Value: "array", Type: v.Type(), Offset: int64(d.off)})
		d.off--
		d.next()
		return
	// v的类型需要是Array或Slice
	case reflect.Array:
	case reflect.Slice:
		break
	}

	i := 0
	for {
		// Look ahead for ] - can only happen on first iteration.
		op := d.scanWhile(scanSkipSpace)
		if op == scanEndArray {
			break
		}

		// Back up so d.value can have the byte we just read.
		d.off--
		d.scan.undo(op)

		// Get element of array, growing if necessary.
		// 确保有足够的空间
		if v.Kind() == reflect.Slice {
			// Grow slice if necessary
			if i >= v.Cap() {
				newcap := v.Cap() + v.Cap() / 2
				if newcap < 4 {
					newcap = 4
				}
				newv := reflect.MakeSlice(v.Type(), v.Len(), newcap)
				reflect.Copy(newv, v)
				v.Set(newv)
			}
			if i >= v.Len() {
				v.SetLen(i + 1)
			}
		}

		// 解析value, 并且设置到array[i]上
		if i < v.Len() {
			// Decode into element.
			d.value(v.Index(i))
		} else {
			// Ran out of fixed array: skip.
			d.value(reflect.Value{})
		}
		i++

		// Next token must be , or ].
		op = d.scanWhile(scanSkipSpace)

		// 下一个指令:
		//    要么数组结束，要么继续下一个元素
		if op == scanEndArray {
			break
		}
		if op != scanArrayValue {
			d.error(errPhase)
		}
	}

	// 实际长度 vs. 期待长度
	// slice 可以调整, array则设置为null value
	if i < v.Len() {
		if v.Kind() == reflect.Array {
			// Array. Zero the rest.
			z := reflect.Zero(v.Type().Elem())
			for ; i < v.Len(); i++ {
				v.Index(i).Set(z)
			}
		} else {
			v.SetLen(i)
		}
	}

	// 如果为空，则设置为size-0 slice
	if i == 0 && v.Kind() == reflect.Slice {
		v.Set(reflect.MakeSlice(v.Type(), 0, 0))
	}
}

var nullLiteral = []byte("null")
var textUnmarshalerType = reflect.TypeOf(new(encoding.TextUnmarshaler)).Elem()

// object consumes an object from d.data[d.off-1:], decoding into the value v.
// the first byte ('{') of the object has been read already.
// JSON解码一个Object对象
//    对象体现在: Map, Struct上
//
func (d *decodeState) object(v reflect.Value) {

	// Check for unmarshaler.
	u, ut, pv := d.indirect(v, false)

	// 通过 Unmarshaler 和 encoding.TextUnmarshaler 接口来解码数据
	if u != nil {
		d.off--
		err := u.UnmarshalJSON(d.next())
		if err != nil {
			d.error(err)
		}
		return
	}
	if ut != nil {
		d.saveError(&UnmarshalTypeError{Value: "object", Type: v.Type(), Offset: int64(d.off)})
		d.off--
		d.next() // skip over { } in input
		return
	}
	v = pv

	// 如果是一个没有方法的Interface, 和interface{}一样，那么就是一个任意类型的对象
	// 例如: map[string]interface{}的value部分
	if v.Kind() == reflect.Interface && v.NumMethod() == 0 {
		// 如果没有方法的interface, 则
		v.Set(reflect.ValueOf(d.objectInterface()))
		return
	}

	// 解码一个对象:
	//     Map, Struct
	// Check type of target:
	//   struct or
	//   map[T1]T2 where T1 is string, an integer type,
	//             or an encoding.TextUnmarshaler
	switch v.Kind() {
	case reflect.Map:
		// Map key must either have string kind, have an integer kind,
		// or be an encoding.TextUnmarshaler.
		t := v.Type() // Map类型的Key
		switch t.Key().Kind() {
		// 基本类型
		case reflect.String,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		default:
			// 或者实现 textUnmarshalerType
			if !reflect.PtrTo(t.Key()).Implements(textUnmarshalerType) {
				d.saveError(&UnmarshalTypeError{Value: "object", Type: v.Type(), Offset: int64(d.off)})
				d.off--
				d.next() // skip over { } in input
				return
			}
		}
		// 如果是空对象，则通过反射创建一个Map
		if v.IsNil() {
			v.Set(reflect.MakeMap(t))
		}
	case reflect.Struct:
	// ok
	default:
		d.saveError(&UnmarshalTypeError{Value: "object", Type: v.Type(), Offset: int64(d.off)})
		d.off--
		d.next() // skip over { } in input
		return
	}

	// 上面: 排除错误，验证Key的类型，初始化Map
	var mapElem reflect.Value

	for {
		// Read opening " of string key or closing }.
		op := d.scanWhile(scanSkipSpace)
		if op == scanEndObject {
			// closing } - can only happen on first iteration.
			break
		}
		if op != scanBeginLiteral {
			d.error(errPhase)
		}

		// Read key.
		// http://imweb.io/topic/57b5f69373ac222929653f23
		// JSON标准中的Key都是字符串
		start := d.off - 1
		op = d.scanWhile(scanContinue)
		item := d.data[start : d.off - 1]
		// 读取有效的Key(类型暂定为字符串）
		key, ok := unquoteBytes(item)
		if !ok {
			d.error(errPhase)
		}

		// Figure out field corresponding to key.
		var subv reflect.Value
		destring := false // whether the value is wrapped in a string to be decoded first

		if v.Kind() == reflect.Map {
			// 如果是Map, 接下来关注Value的类型
			elemType := v.Type().Elem()
			if !mapElem.IsValid() {
				mapElem = reflect.New(elemType).Elem()
			} else {
				mapElem.Set(reflect.Zero(elemType))
			}
			subv = mapElem
		} else {
			// 如果struct, 则读取v的所有的Fields

			// 1. 定位名字为key的Field
			var f *field
			fields := cachedTypeFields(v.Type())
			for i := range fields {
				ff := &fields[i]
				if bytes.Equal(ff.nameBytes, key) {
					f = ff
					break
				}
				if f == nil && ff.equalFold(ff.nameBytes, key) {
					f = ff
				}
			}

			if f != nil {
				// 从顶层对象出发，设置好Field的位置关系，以及初始化
				subv = v
				// 判断是否为destring
				destring = f.inQuoted
				for _, i := range f.index {
					// subv要指向 v的子元素
					subv = subv.Field(i)
					// log.Printf("subv is %v, change to Elem: %s, i: %d", subv.Kind(), f.name, i)
					if subv.Kind() == reflect.Ptr {
						if subv.IsNil() {
							subv.Set(reflect.New(subv.Type().Elem()))
						}
						subv = subv.Elem()
					}
				}

				// 当前的Context
				d.errorContext.Field = f.name
				d.errorContext.Struct = v.Type().Name()
			}
		}

		// Read : before value.
		if op == scanSkipSpace {
			op = d.scanWhile(scanSkipSpace)
		}
		if op != scanObjectKey {
			d.error(errPhase)
		}

		// 现在需要解析Value: subv
		// 如果是quoted, 那么需要解析字符串: valueQuoted
		if destring {
			d.parseQuotedValue(subv)
		} else {
			// 读取subv，按照具体类型来读取
			d.value(subv)
		}

		// 得到了 key, value
		// 然后放置在Map, 或 struct中
		// Write value back to map;
		// if using struct, subv points into struct already.
		if v.Kind() == reflect.Map {
			// 将key 转换成为 具体的数据类型
			kt := v.Type().Key()
			var kv reflect.Value

			switch {
			case kt.Kind() == reflect.String:
				// 1. []byte --> string
				kv = reflect.ValueOf(key).Convert(kt)
			case reflect.PtrTo(kt).Implements(textUnmarshalerType):
				// 2. textUnmarshalerType 接口，自己可以直接转换成为string
				kv = reflect.New(v.Type().Key())
				d.literalStore(item, kv, true)
				kv = kv.Elem()
			default:
				// 3. 其他情况，作为key只能是int相关的参数了
				kv1, err1 := d.parseIntKeyValue(kt, key, start)
				if err1 != nil {
					d.saveError(err1)
					return
				}
				kv = *kv1
			}
			v.SetMapIndex(kv, subv)
		}

		// Next token must be , or }.
		op = d.scanWhile(scanSkipSpace)

		// 要不对象结束，要么当前的Value结束，开始下一个Value
		if op == scanEndObject {
			break
		}
		if op != scanObjectValue {
			d.error(errPhase)
		}

		// Context清空，准备下一个数据的解析
		d.errorContext.Struct = ""
		d.errorContext.Field = ""
	}
}

//
// 根据 kt 的类型，将key转换成为 int
// Only strings, floats, integers, and booleans can be quoted.
//
func (d *decodeState) parseQuotedValue(subv reflect.Value) {

	// 得到value, 如何立即呢?
	switch qv := d.valueQuoted().(type) {
	case nil:
		d.literalStore(nullLiteral, subv, false)
		return
	case string:
		// 如果是string
		d.literalStore([]byte(qv), subv, true)
		return
	case bool:
		if subv.Type().Kind() == reflect.Bool {
			subv.SetBool(qv)
			return
		}
	case float64:
		switch subv.Type().Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			subv.SetInt(int64(qv))
			return
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			subv.SetUint(uint64(qv))
			return
		case reflect.Float32, reflect.Float64:
			subv.SetFloat(qv)
			return
		}
	//
	// 其他类型
	// floats, integers, and booleans
	// 也做简单的兼容
	}
	d.saveError(fmt.Errorf("json: invalid use of ,string struct tag, trying to unmarshal unquoted value into %v", subv.Type()))
}

//
// 根据 kt 的类型，将key转换成为 int
//
func (d *decodeState) parseIntKeyValue(kt reflect.Type, key []byte, start int) (*reflect.Value, *UnmarshalTypeError) {
	var kv reflect.Value
	switch kt.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		s := string(key)
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || reflect.Zero(kt).OverflowInt(n) {
			return nil, &UnmarshalTypeError{Value: "number " + s, Type: kt, Offset: int64(start + 1)}
		}
		kv = reflect.ValueOf(n).Convert(kt)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		s := string(key)
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil || reflect.Zero(kt).OverflowUint(n) {
			return nil, &UnmarshalTypeError{Value: "number " + s, Type: kt, Offset: int64(start + 1)}
		}
		kv = reflect.ValueOf(n).Convert(kt)
	default:
		log.Printf("parseIntKeyValue invalid key")
		panic("json: Unexpected key type") // should never occur
	}
	return &kv, nil
}

// literal consumes a literal from d.data[d.off-1:], decoding into the value v.
// The first byte of the literal has been read already
// (that's how the caller knows it's a literal).
func (d *decodeState) literal(v reflect.Value) {
	// All bytes inside literal return scanContinue op code.
	start := d.off - 1
	op := d.scanWhile(scanContinue)

	// Scan read one byte too far; back up.
	d.off--
	d.scan.undo(op)

	d.literalStore(d.data[start:d.off], v, false)
}

// convertNumber converts the number literal s to a float64 or a Number
// depending on the setting of d.useNumber.
func (d *decodeState) convertNumber(s string) (interface{}, error) {

	// 按照64 bitfloat来处理的
	// TODO: 如果没有特殊字符，就可以按照int64来处理
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, &UnmarshalTypeError{Value: "number " + s, Type: reflect.TypeOf(0.0), Offset: int64(d.off)}
	}
	return f, nil
}

// literalStore decodes a literal stored in item into v.
//
// fromQuoted indicates whether this literal came from unwrapping a
// string from the ",string" struct tag option. this is used only to
// produce more helpful error messages.
func (d *decodeState) literalStore(item []byte, v reflect.Value, fromQuoted bool) {

	if len(item) == 0 {
		//Empty string given
		//item必须非空
		d.saveError(fmt.Errorf("json: invalid use of ,string struct tag, trying to unmarshal %q into %v", item, v.Type()))
		return
	}

	// item --> v
	// 考虑类型，考虑 fromQuoted
	//
	isNull := item[0] == 'n' // null
	u, ut, pv := d.indirect(v, isNull)

	if u != nil {
		err := u.UnmarshalJSON(item)
		if err != nil {
			d.error(err)
		}
		return
	}
	if ut != nil {
		if item[0] != '"' {
			if fromQuoted {
				d.saveError(fmt.Errorf("json: invalid use of ,string struct tag, trying to unmarshal %q into %v", item, v.Type()))
			} else {
				var val string
				switch item[0] {
				case 'n':
					val = "null"
				case 't', 'f':
					val = "bool"
				default:
					val = "number"
				}
				d.saveError(&UnmarshalTypeError{Value: val, Type: v.Type(), Offset: int64(d.off)})
			}
			return
		}
		s, ok := unquoteBytes(item)
		if !ok {
			log.Printf("111111111111")
			if fromQuoted {
				d.error(fmt.Errorf("json: invalid use of ,string struct tag, trying to unmarshal %q into %v", item, v.Type()))
			} else {
				d.error(errPhase)
			}
		}
		err := ut.UnmarshalText(s)
		if err != nil {
			d.error(err)
		}
		return
	}

	v = pv
	switch c := item[0]; c {
	case 'n': // null
		// The main parser checks that only true and false can reach here,
		// but if this was a quoted string input, it could be anything.
		if fromQuoted && string(item) != "null" {
			d.saveError(fmt.Errorf("json: invalid use of ,string struct tag, trying to unmarshal %q into %v", item, v.Type()))
			break
		}
		switch v.Kind() {
		case reflect.Interface, reflect.Ptr, reflect.Map, reflect.Slice:
			v.Set(reflect.Zero(v.Type()))
		// otherwise, ignore null for primitives/string
		}
	case 't', 'f': // true, false
		value := item[0] == 't'
		// The main parser checks that only true and false can reach here,
		// but if this was a quoted string input, it could be anything.
		if fromQuoted && string(item) != "true" && string(item) != "false" {
			d.saveError(fmt.Errorf("json: invalid use of ,string struct tag, trying to unmarshal %q into %v", item, v.Type()))
			break
		}

		// 解析出来的是: true/false
		switch v.Kind() {
		default:
			// 其他情况下，报不兼容的错误
			if fromQuoted {
				d.saveError(fmt.Errorf("json: invalid use of ,string struct tag, trying to unmarshal %q into %v", item, v.Type()))
			} else {
				d.saveError(&UnmarshalTypeError{Value: "bool", Type: v.Type(), Offset: int64(d.off)})
			}
		case reflect.Bool:
			// 直接设置
			v.SetBool(value)
		case reflect.Interface:
			// interface{}, 任意的，直接设置
			if v.NumMethod() == 0 {
				v.Set(reflect.ValueOf(value))
			} else {
				// 不匹配
				d.saveError(&UnmarshalTypeError{Value: "bool", Type: v.Type(), Offset: int64(d.off)})
			}
		}

	case '"': // string： "xxxx"
		s, ok := unquoteBytes(item)
		if !ok {
			if fromQuoted {
				d.error(fmt.Errorf("json: invalid use of ,string struct tag, trying to unmarshal %q into %v", item, v.Type()))
			} else {
				d.error(errPhase)
			}
		}

		// 字符串 s --> v.Kind()
		switch v.Kind() {
		default:
			d.parseIntValue(c, item, &v, fromQuoted)
		// d.saveError(&UnmarshalTypeError{Value: "string", Type: v.Type(), Offset: int64(d.off)})
		case reflect.Slice:
			// 目标是: []byte
			if v.Type().Elem().Kind() != reflect.Uint8 {
				d.saveError(&UnmarshalTypeError{Value: "string", Type: v.Type(), Offset: int64(d.off)})
				break
			}
			// []byte base64编码
			b := make([]byte, base64.StdEncoding.DecodedLen(len(s)))
			n, err := base64.StdEncoding.Decode(b, s)
			if err != nil {
				d.saveError(err)
				break
			}
			v.SetBytes(b[:n])

		case reflect.String:
			// 目标是字符串，则直接设置
			v.SetString(string(s))
		case reflect.Interface:
			if v.NumMethod() == 0 {
				// interface{}, 任意类型，就作为string
				v.Set(reflect.ValueOf(string(s)))
			} else {
				// 其他类型，报错
				d.saveError(&UnmarshalTypeError{Value: "string", Type: v.Type(), Offset: int64(d.off)})
			}
		}

	default: // number
		// "key": 1234
		// 解析1234
		d.parseIntValue(c, item, &v, fromQuoted)
	}
}

func (d *decodeState) parseIntValue(c byte, item []byte, v *reflect.Value, fromQuoted bool) {
	// log.Printf("parseIntValue, item: %s", string(item))
	if c != '-' && (c < '0' || c > '9') {
		if fromQuoted {
			d.error(fmt.Errorf("json: invalid use of ,string struct tag, trying to unmarshal %q into %v", item, v.Type()))
		} else {
			d.error(errPhase)
		}
	}
	s := string(item)
	switch v.Kind() {
	default:
		// log.Printf("parseIntValue, default item: %s", string(item))
		if fromQuoted {
			d.error(fmt.Errorf("json: invalid use of ,string struct tag, trying to unmarshal %q into %v", item, v.Type()))
		} else {
			d.error(&UnmarshalTypeError{Value: "number", Type: v.Type(), Offset: int64(d.off)})
		}
	case reflect.Interface:
		n, err := d.convertNumber(s)
		if err != nil {
			d.saveError(err)
			break
		}
		if v.NumMethod() != 0 {
			d.saveError(&UnmarshalTypeError{Value: "number", Type: v.Type(), Offset: int64(d.off)})
			break
		}
		v.Set(reflect.ValueOf(n))

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v.OverflowInt(n) {
			d.saveError(&UnmarshalTypeError{Value: "number " + s, Type: v.Type(), Offset: int64(d.off)})
			break
		}
		v.SetInt(n)

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil || v.OverflowUint(n) {
			d.saveError(&UnmarshalTypeError{Value: "number " + s, Type: v.Type(), Offset: int64(d.off)})
			break
		}
		v.SetUint(n)

	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil || v.OverflowFloat(n) {
			d.saveError(&UnmarshalTypeError{Value: "number " + s, Type: v.Type(), Offset: int64(d.off)})
			break
		}
		v.SetFloat(n)
	}
	// log.Printf("parseIntValue, succeed %s", string(item))
}

// The xxxInterface routines build up a value to be stored
// in an empty interface. They are not strictly necessary,
// but they avoid the weight of reflection in this common case.

// valueInterface is like value but returns interface{}
func (d *decodeState) valueInterface() interface{} {
	switch d.scanWhile(scanSkipSpace) {
	default:
		d.error(errPhase)
		log.Printf("valueInterface unreachable")
		panic("unreachable")
	// 遍历三类对象:
	// array, object, 以及literal(不可拆分的object)
	case scanBeginArray:
		return d.arrayInterface()
	case scanBeginObject:
		return d.objectInterface()
	case scanBeginLiteral:
		return d.literalInterface()
	}
}

//
// 解析任意类似的数组
//
// arrayInterface is like array but returns []interface{}.
func (d *decodeState) arrayInterface() []interface{} {
	var v = make([]interface{}, 0)
	for {
		// Look ahead for ] - can only happen on first iteration.
		op := d.scanWhile(scanSkipSpace)
		if op == scanEndArray {
			break
		}

		// Back up so d.value can have the byte we just read.
		d.off--
		d.scan.undo(op)

		v = append(v, d.valueInterface())

		// Next token must be , or ].
		op = d.scanWhile(scanSkipSpace)
		if op == scanEndArray {
			break
		}
		if op != scanArrayValue {
			d.error(errPhase)
		}
	}
	return v
}

// objectInterface is like object but returns map[string]interface{}.
// interface在go中表示任意类型，并不是接口啥的?
// 没有类型要求，则就自由处理了
//
// 解析任意类似的数组
//
func (d *decodeState) objectInterface() map[string]interface{} {
	m := make(map[string]interface{})
	for {
		// Read opening " of string key or closing }.
		op := d.scanWhile(scanSkipSpace)

		// 出现有意义的信息
		// 要么结束
		// 要么出现Literal
		if op == scanEndObject {
			// closing } - can only happen on first iteration.
			break
		}
		if op != scanBeginLiteral {
			d.error(errPhase)
		}

		// Read string key.
		start := d.off - 1
		op = d.scanWhile(scanContinue)
		item := d.data[start : d.off - 1]
		key, ok := unquote(item)
		if !ok {
			d.error(errPhase)
		}

		// Read : before value.
		if op == scanSkipSpace {
			op = d.scanWhile(scanSkipSpace)
		}

		// key结束
		if op != scanObjectKey {
			d.error(errPhase)
		}

		// Read value.
		m[key] = d.valueInterface()

		// Next token must be , or }.
		op = d.scanWhile(scanSkipSpace)
		if op == scanEndObject {
			break
		}
		if op != scanObjectValue {
			d.error(errPhase)
		}
	}
	return m
}

// literalInterface is like literal but returns an interface value.
//
// 解析 literal
//
func (d *decodeState) literalInterface() interface{} {
	// All bytes inside literal return scanContinue op code.
	start := d.off - 1
	op := d.scanWhile(scanContinue)

	// Scan read one byte too far; back up.
	d.off--
	d.scan.undo(op)
	item := d.data[start:d.off]

	// 如何理解literal呢?
	// literal 没有类型限制，自动判断
	// nil
	// true
	// false
	// "xxxx"
	switch c := item[0]; c {
	case 'n': // null
		return nil

	case 't', 'f': // true, false
		return c == 't'

	case '"': // string
		s, ok := unquote(item)
		if !ok {
			d.error(errPhase)
		}
		return s

	default: // number
		if c != '-' && (c < '0' || c > '9') {
			d.error(errPhase)
		}
		n, err := d.convertNumber(string(item))
		if err != nil {
			d.saveError(err)
		}
		return n
	}
}

// getu4 decodes \uXXXX from the beginning of s, returning the hex value,
// or it returns -1.
func getu4(s []byte) rune {
	if len(s) < 6 || s[0] != '\\' || s[1] != 'u' {
		return -1
	}
	r, err := strconv.ParseUint(string(s[2:6]), 16, 64)
	if err != nil {
		return -1
	}
	return rune(r)
}

// unquote converts a quoted JSON string literal s into an actual string t.
// The rules are different than for Go, so cannot use strconv.Unquote.
func unquote(s []byte) (t string, ok bool) {
	s, ok = unquoteBytes(s)
	t = string(s)
	return
}

//
// 期待: s 是一个字符串，需要将前后的""去掉
//
func unquoteBytes(s []byte) (t []byte, ok bool) {
	// 无效的数据，返回 nil, false
	if len(s) < 2 || s[0] != '"' || s[len(s) - 1] != '"' {
		return
	}
	// 去掉收尾"
	s = s[1 : len(s) - 1]

	// Check for unusual characters. If there are none,
	// then no unquoting is needed, so return a slice of the
	// original bytes.
	r := 0
	for r < len(s) {
		c := s[r]
		// 一旦发现异常数据，则退出检查流程
		if c == '\\' || c == '"' || c < ' ' {
			break
		}
		if c < utf8.RuneSelf {
			r++
			continue
		}
		rr, size := utf8.DecodeRune(s[r:])
		if rr == utf8.RuneError && size == 1 {
			break
		}
		r += size
	}

	// 都是简单字符，直接返回
	if r == len(s) {
		return s, true
	}

	// 处理特殊字符
	b := make([]byte, len(s) + 2 * utf8.UTFMax)
	w := copy(b, s[0:r])
	for r < len(s) {
		// Out of room?  Can only happen if s is full of
		// malformed UTF-8 and we're replacing each
		// byte with RuneError.
		if w >= len(b) - 2 * utf8.UTFMax {
			nb := make([]byte, (len(b) + utf8.UTFMax) * 2)
			copy(nb, b[0:w])
			b = nb
		}
		switch c := s[r]; {
		case c == '\\':
			r++
			if r >= len(s) {
				return
			}
			switch s[r] {
			default:
				return
			case '"', '\\', '/', '\'':
				b[w] = s[r]
				r++
				w++
			case 'b':
				b[w] = '\b'
				r++
				w++
			case 'f':
				b[w] = '\f'
				r++
				w++
			case 'n':
				b[w] = '\n'
				r++
				w++
			case 'r':
				b[w] = '\r'
				r++
				w++
			case 't':
				b[w] = '\t'
				r++
				w++
			case 'u':
				r--
				rr := getu4(s[r:])
				if rr < 0 {
					return
				}
				r += 6
				if utf16.IsSurrogate(rr) {
					rr1 := getu4(s[r:])
					if dec := utf16.DecodeRune(rr, rr1); dec != unicode.ReplacementChar {
						// A valid pair; consume.
						r += 6
						w += utf8.EncodeRune(b[w:], dec)
						break
					}
					// Invalid surrogate; fall back to replacement rune.
					rr = unicode.ReplacementChar
				}
				w += utf8.EncodeRune(b[w:], rr)
			}

		// Quote, control characters are invalid.
		case c == '"', c < ' ':
			return

		// ASCII
		case c < utf8.RuneSelf:
			b[w] = c
			r++
			w++

		// Coerce to well-formed UTF-8.
		default:
			rr, size := utf8.DecodeRune(s[r:])
			r += size
			w += utf8.EncodeRune(b[w:], rr)
		}
	}
	return b[0:w], true
}
