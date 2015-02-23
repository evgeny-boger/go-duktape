package duktape

/*
#cgo linux LDFLAGS: -lm

# include "duktape.h"
extern duk_ret_t goFinalize(duk_context *ctx);
extern duk_ret_t goCall(duk_context *ctx);
*/
import "C"
import "log"
import "sync"
import "errors"
import "unsafe"

const goFuncProp = "goFuncData"
const goObjProp = "goObjData"
const (
	DUK_ENUM_INCLUDE_NONENUMERABLE	 = C.DUK_ENUM_INCLUDE_NONENUMERABLE
	DUK_ENUM_INCLUDE_INTERNAL	 = C.DUK_ENUM_INCLUDE_INTERNAL
	DUK_ENUM_OWN_PROPERTIES_ONLY	 = C.DUK_ENUM_OWN_PROPERTIES_ONLY
	DUK_ENUM_ARRAY_INDICES_ONLY	 = C.DUK_ENUM_ARRAY_INDICES_ONLY
	DUK_ENUM_SORT_ARRAY_INDICES	 = C.DUK_ENUM_SORT_ARRAY_INDICES
	DUK_ENUM_NO_PROXY_BEHAVIOR	 = C.DUK_ENUM_NO_PROXY_BEHAVIOR
)

const (
	DUK_TYPE_NONE Type = iota
	DUK_TYPE_UNDEFINED
	DUK_TYPE_NULL
	DUK_TYPE_BOOLEAN
	DUK_TYPE_NUMBER
	DUK_TYPE_STRING
	DUK_TYPE_OBJECT
	DUK_TYPE_BUFFER
	DUK_TYPE_POINTER
)

type Type int

func (t Type) IsNone() bool      { return t == DUK_TYPE_NONE }
func (t Type) IsUndefined() bool { return t == DUK_TYPE_UNDEFINED }
func (t Type) IsNull() bool      { return t == DUK_TYPE_NULL }
func (t Type) IsBool() bool      { return t == DUK_TYPE_BOOLEAN }
func (t Type) IsNumber() bool    { return t == DUK_TYPE_NUMBER }
func (t Type) IsString() bool    { return t == DUK_TYPE_STRING }
func (t Type) IsObject() bool    { return t == DUK_TYPE_OBJECT }
func (t Type) IsBuffer() bool    { return t == DUK_TYPE_BUFFER }
func (t Type) IsPointer() bool   { return t == DUK_TYPE_POINTER }

var objectMutex sync.Mutex
var objectMap map[unsafe.Pointer]interface{} = make(map[unsafe.Pointer]interface{})

type Context struct {
	duk_context unsafe.Pointer
}

// Returns initialized duktape context object
func NewContext() *Context {
	ctx := &Context{
		// TODO: "A caller SHOULD implement a fatal error handler in most applications."
		duk_context: C.duk_create_heap(nil, nil, nil, nil, nil),
	}
	return ctx
}

func (d *Context) PutInternalPropString (objIndex int, key string) bool {
	cKey := C.CString("_" + key)
	defer C.free(unsafe.Pointer(cKey))
	*cKey = -1 // \xff as the first char designates an internal property
	return int(C.duk_put_prop_string(d.duk_context, C.duk_idx_t(objIndex), cKey)) == 1
}

func (d *Context) GetInternalPropString (objIndex int, key string) bool {
	cKey := C.CString("_" + key)
	defer C.free(unsafe.Pointer(cKey))
	*cKey = -1 // \xff as the first char designates an internal property
	return int(C.duk_get_prop_string(d.duk_context, C.duk_idx_t(objIndex), cKey)) == 1
}

//export goFinalize
func goFinalize(ctx unsafe.Pointer) C.duk_ret_t {
	d := &Context{ctx}
	d.PushCurrentFunction()
	d.GetInternalPropString(-1, goFuncProp)	
	if !Type(d.GetType(-1)).IsPointer() {
		d.Pop2()
		log.Printf("finalize -- fail!")
		return C.duk_ret_t(C.DUK_RET_TYPE_ERROR)
	}
	key := d.GetPointer(-1)
	log.Printf("finalize: %v", key)
	d.Pop2()
	objectMutex.Lock()
	delete(objectMap, key)
	objectMutex.Unlock()
	C.free(key)
	return C.duk_ret_t(0)
}

func (d *Context) putGoObjectRef(prop string, o interface{}) {
	key := C.malloc(1) // guaranteed to be unique until freed

	objectMutex.Lock()
	objectMap[key] = o
	objectMutex.Unlock()
	log.Printf("new object ref via %s: %v = %v", prop, key, o)

	d.PushCFunction((*[0]byte)(C.goFinalize), 1)
	d.PushPointer(key)
	d.PutInternalPropString(-2, goFuncProp)

	d.SetFinalizer(-2)

	d.PushPointer(key)
	d.PutInternalPropString(-2, prop)
}

func (d *Context) PushGoObject(o interface{}) {
	d.PushObject()
	d.putGoObjectRef(goObjProp, o)
}

func (d *Context) getGoObjectRef(prop string) interface{} {
	d.GetInternalPropString(-1, prop)
	if !Type(d.GetType(-1)).IsPointer() {
		d.Pop()
		return nil
	}
	key := d.GetPointer(-1)
	d.Pop()
	objectMutex.Lock()
	defer objectMutex.Unlock()
	log.Printf("get ref via %s: %v = %v", prop, key, objectMap[key])
	return objectMap[key]
}

func (d *Context) GetGoObject() interface{} {
	return d.getGoObjectRef(goObjProp)
}

//export goCall
func goCall(ctx unsafe.Pointer) C.duk_ret_t {
	d := &Context{ctx}

	/*
	d.PushContextDump()
	log.Printf("goCall context: %s", d.GetString(-1))
	d.Pop()
        */

	d.PushCurrentFunction()
	if fd, _ := d.getGoObjectRef(goFuncProp).(*GoFuncData); fd == nil {
		d.Pop()
		return C.duk_ret_t(C.DUK_RET_TYPE_ERROR)
	} else {
		d.Pop()
		return C.duk_ret_t(fd.f(d))
	}
}

type GoFunc func (d *Context) int
type GoFuncData struct {
	f GoFunc
}

// Push goCall with its "goFuncData" property set to fd
func (d *Context) PushGoFunc(fd *GoFuncData) {
	d.PushCFunction((*[0]byte)(C.goCall), C.DUK_VARARGS)
	d.putGoObjectRef(goFuncProp, fd)
}


type MethodSuite map[string]GoFunc

func (d *Context) EvalWith(source string, suite MethodSuite) error {
	if err := d.PevalString(source); err != 0 {
		return errors.New(d.SafeToString(-1))
	}

	d.PushObject()

	// Make sure we keep references to all the GoFuncData
	suiteData := make(map[string]*GoFuncData)
	for prop, f := range suite {
		suiteData[prop] = &GoFuncData{f}
	}

	for prop, fd := range suiteData {
		d.PushGoFunc(fd)
		d.PutPropString(-2, prop)
	}

	if err := d.Pcall(1); err != 0 {
		return errors.New(d.SafeToString(-1))
	}

	return nil
}
