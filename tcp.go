package next

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"reflect"
	"runtime"
	"time"
)

type Tcp struct {
	Conn       map[string]*net.TCPConn
	Config     *Config
	Logger     *log.Logger
	routes     *Routes
	middleware []reflect.Value
}

const (
	TcpHead       = 0xAA
	TcpTail       = 0x0A
	TcpMaxContent = 2 << 10
)

func NewTcp() *Tcp {
	tcp := &Tcp{
		Conn:       make(map[string]*net.TCPConn),
		Config:     NewConfig(),
		Logger:     log.New(os.Stdout, "", log.Ldate|log.Ltime),
		routes:     NewRoutes(),
		middleware: make([]reflect.Value, 0),
	}

	// Load default config if exists
	file := "config.json"
	if fileExists(file) {
		tcp.Config.Load(file)
	}

	return tcp
}

// Pack is a utility function to read from the supplied Writer
// according to the Next protocol spec:
//
//    AA[x][x][x][x][x][x][x][x]55
//      |  (int32) || (binary)
//      |  4-byte  || N-byte
//      ------------------------...
//        size       data
func (t *Tcp) Pack(w io.Writer, data []byte) error {
	// Head
	_, err := w.Write([]byte{TcpHead})
	if err != nil {
		return errors.New("write head fail")
	}

	// Size
	err = binary.Write(w, binary.LittleEndian, int32(len(data))+6)
	if err != nil {
		return errors.New("write size fail")
	}

	// Data
	_, err = w.Write(data)
	if err != nil {
		return errors.New("write data fail")
	}

	// Tail
	_, err = w.Write([]byte{TcpTail})
	if err != nil {
		return errors.New("write tail fail")
	}

	return nil
}

// Unpack is a utility function to read from the supplied Reader
// according to the Next protocol spec:
//
//    AA[x][x][x][x][x][x][x][x]55
//      |  (int32) || (binary)
//      |  4-byte  || N-byte
//      ------------------------...
//        size       data
func (t *Tcp) Unpack(r io.Reader) ([]byte, error) {
	// Check head
	head := make([]byte, 1)
	_, err := r.Read(head)
	if err != nil {
		return nil, err
	}
	if int32(head[0]) != TcpHead {
		return nil, errors.New("data head is error")
	}

	// message size
	var size int32
	err = binary.Read(r, binary.LittleEndian, &size)
	if err != nil {
		return nil, err
	}
	size = size - 6
	if size <= 0 || size > TcpMaxContent {
		return nil, errors.New("data size is error")
	}

	// message binary data
	buf := make([]byte, size)
	_, err = io.ReadFull(r, buf)
	if err != nil {
		return nil, err
	}

	// Check head
	tail := make([]byte, 1)
	_, err = r.Read(tail)
	if err != nil {
		return nil, err
	}
	if int32(tail[0]) != TcpTail {
		return nil, errors.New("data tail is error")
	}

	return buf, nil
}

func (t *Tcp) handler(conn *net.TCPConn, body []byte) {
	// Read json body
	json := NewJson()
	json.Load(body)

	// Add seq flag from client
	requestPath := json.Get("method").MustString()

	ctx := TcpContext{
		Method: requestPath,
		Params: make(map[string]string),
		Tcp:    t,
		Fd:     t.Fd(conn),
		conn:   conn,
	}
	tm := time.Now().UTC()
	defer t.logRequest(ctx, tm)

	ctx.Params["seq"] = json.Get("seq").MustString()
	route := t.routes.Match(requestPath, "VIA")
	if route == nil {
		ctx.WriteJSON("404", "request method not found")
		return
	}
	ctx.Params["method"] = requestPath

	if data, err := json.Get("data").Map(); err == nil {
		if len(data) > 0 {
			for key, val := range data {
				v, err := val.(string)
				if !err {
					ctx.WriteJSON("403", "data is not map[string]string")
					return
				}
				ctx.Params[key] = v
			}
		}
	}

	cr := route.cr

	var args []reflect.Value
	handlerType := route.handler.Type()
	if requiresTcpContext(handlerType) {
		args = append(args, reflect.ValueOf(&ctx))
	}

	match := cr.FindStringSubmatch(requestPath)
	for _, arg := range match[1:] {
		args = append(args, reflect.ValueOf(arg))
	}

	_, err := t.safelyCall(route.handler, args)
	if err != nil {
		//there was an error or panic while calling the handler
		ctx.WriteJSON("500", "Server Error")
	}
}

// Get the integer Unix file descriptor referencing the open file
func (t *Tcp) Fd(conn *net.TCPConn) string {
	return conn.RemoteAddr().String()
}

func (t *Tcp) Pipe(conn *net.TCPConn) {
	fd := t.Fd(conn)
	defer func() {
		t.Logger.Printf("disconnected: %s\n", fd)
		conn.Close()
		delete(t.Conn, fd)
	}()

	// Save in map
	t.Conn[fd] = conn

	// Read data
	reader := bufio.NewReader(conn)
	for {
		body, err := t.Unpack(reader)
		if err != nil {
			if err == io.EOF {
				return
			}
			t.Logger.Print(err)
		}

		// Filter heart pack
		if string(body) != "hello" {
			t.handler(conn, body)
		}
		reader.Reset(conn)
	}
}

func (t *Tcp) Run(addr string) {
	tcpAddr, _ := net.ResolveTCPAddr("tcp", addr)
	tcpListener, _ := net.ListenTCP("tcp", tcpAddr)
	defer tcpListener.Close()

	t.Logger.Printf("next tcp serving %s\n", addr)

	// Run middleware
	go func() {
		for _, v := range t.middleware {
			var args []reflect.Value
			args = append(args, reflect.ValueOf(t))
			t.safelyCall(v, args)
		}
	}()

	// Run tcp listener
	for {
		tcpConn, err := tcpListener.AcceptTCP()
		if err != nil {
			continue
		}

		t.Logger.Printf("connected: %s\n", tcpConn.RemoteAddr().String())
		go t.Pipe(tcpConn)
	}
}

// Post adds a handler for the 'Via' TCP method for tcp.
func (t *Tcp) Middleware(handler interface{}) {
	switch handler.(type) {
	case reflect.Value:
		fv := handler.(reflect.Value)
		t.middleware = append(t.middleware, fv)
	default:
		fv := reflect.ValueOf(handler)
		t.middleware = append(t.middleware, fv)
	}
}

// Post adds a handler for the 'Via' TCP method for tcp.
func (t *Tcp) Via(route string, handler interface{}) {
	t.routes.Add(route, "VIA", handler)
}

func (t *Tcp) WriteJSON(conn *net.TCPConn, code, msg string, data ...interface{}) {
	json := NewJson()
	json.Set("code", code)
	json.Set("msg", msg)

	if len(data) > 0 {
		json.Set("data", data[0])
	}
	out, _ := json.Encode()

	t.Pack(conn, out)
}

// safelyCall invokes `function` in recover block
func (t *Tcp) safelyCall(function reflect.Value, args []reflect.Value) (resp []reflect.Value, e interface{}) {
	defer func() {
		if err := recover(); err != nil {
			if t.Config.Bool("panic") {
				// go back to panic
				panic(err)
			} else {
				e = err
				resp = nil
				t.Logger.Println("Handler crashed with error", err)
				for i := 1; ; i += 1 {
					_, file, line, ok := runtime.Caller(i)
					if !ok {
						break
					}
					t.Logger.Println(file, line)
				}
			}
		}
	}()
	return function.Call(args), nil
}

func (t *Tcp) logRequest(ctx TcpContext, sTime time.Time) {
	//log the request
	var logEntry bytes.Buffer

	duration := time.Now().Sub(sTime)

	fmt.Fprintf(&logEntry, "%s - \033[32;1m TCP %s\033[0m - %v", ctx.Fd, ctx.Method, duration)

	if len(ctx.Params) > 0 {
		fmt.Fprintf(&logEntry, " - \033[37;1mParams: %v\033[0m\n", ctx.Params)
	}

	ctx.Tcp.Logger.Print(logEntry.String())
}

// requiresContext determines whether 'handlerType' contains
// an argument to 'web.Ctx' as its first argument
func requiresTcpContext(handlerType reflect.Type) bool {
	//if the method doesn't take arguments, no
	if handlerType.NumIn() == 0 {
		return false
	}

	//if the first argument is not a pointer, no
	a0 := handlerType.In(0)
	if a0.Kind() != reflect.Ptr {
		return false
	}
	//if the first argument is a context, yes
	if a0.Elem() == reflect.TypeOf(TcpContext{}) {
		return true
	}

	return false
}

// --------
// Tcp Context
// --------
type TcpContext struct {
	Method string
	Params map[string]string
	Tcp    *Tcp
	Fd     string
	conn   *net.TCPConn
}

// WriteJSON writes json data into the response object.
func (ctx *TcpContext) WriteJSON(code, msg string, data ...interface{}) {
	if ctx.conn == nil {
		return
	}

	json := NewJson()
	if ctx.Tcp.Config.Bool("debug.profiler") {
		json.Set("debug", ctx.Params)
	}

	// Back client request version
	if seq, ok := ctx.Params["seq"]; ok {
		json.Set("seq", seq)
	}
	// Back request method
	if method, ok := ctx.Params["method"]; ok {
		json.Set("method", method)
	}

	json.Set("code", code)
	json.Set("msg", msg)

	if len(data) > 0 {
		json.Set("data", data[0])
	}
	out, _ := json.Encode()

	ctx.Tcp.Pack(ctx.conn, out)
}
