package binlog

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net"
	"reflect"
	"strings"
	"time"
)

// NullByte is a constant representing a null byte in the MySQL protocol.
const NullByte byte = 0

// MaxPacketSize is the maximum size of a MySQL protocol packet.
const MaxPacketSize = MaxUint16

// TypeNullTerminatedString represents the null terminated string type in the MySQL protocol.
const TypeNullTerminatedString = int(0)

// TypeFixedString represents the fixed string type in the MySQL protocol.
const TypeFixedString = int(1)

// TypeFixedInt represents the fixed integer type in the MySQL protocol.
const TypeFixedInt = int(2)

// TypeLenEncInt represents the length encoded integer type in the MySQL protocol.
const TypeLenEncInt = int(3)

// TypeRestOfPacketString represents the rest of packet string type in the MySQL protocol.
const TypeRestOfPacketString = int(4)

// TypeLenEncString represents the length encoded string type in the MySQL protocol.
const TypeLenEncString = int(5)

// MaxUint08 is the largest unsigned eight byte integer.
const MaxUint08 = 1<<8 - 1

// MaxUint16 is is the largest unsigned sixteen byte integer.
const MaxUint16 = 1<<16 - 1

// MaxUint24 is is the largest unsigned 24 byte integer.
const MaxUint24 = 1<<24 - 1

// MaxUint64 is is the largest unsigned 64 byte integer.
const MaxUint64 = 1<<64 - 1

// StatusOK indicates an OK packet from the MySQL protocol.
const StatusOK = 0x00

// StatusEOF indicates an end of file packet from the MySQL protocol.
const StatusEOF = 0xFE

// StatusErr indicates an error packet from the MySQL protocol.
const StatusErr = 0xFF

// StatusAuth indicates an authorization packet from the MySQL protocol.
const StatusAuth = 0x01

// Config represents the required parameters required to make a MySQL connection.
type Config struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	Pass       string `json:"password"`
	Database   string `json:"database"`
	SSL        bool   `json:"ssl"`
	SSLCA      string `json:"ssl-ca"`
	SSLCer     string `json:"ssl-cer"`
	SSLKey     string `json:"ssl-key"`
	VerifyCert bool   `json:"verify-cert"`
	ServerID   uint64 `json:"server-id"`
	BinlogFile string `json:"binlog-file"`
	Timeout    time.Duration
}

func newBinlogConfig(dsn string) (*Config, error) {
	var err error

	b, err := ioutil.ReadFile(dsn)
	if err != nil {
		return nil, err
	}

	config := Config{}
	err = json.Unmarshal(b, &config)

	return &config, err
}

// Conn represents a connection to a MySQL server.
type Conn struct {
	Config            *Config
	curConn           net.Conn
	tcpConn           *net.TCPConn
	secTCPConn        *tls.Conn
	Handshake         *Handshake
	HandshakeResponse *HandshakeResponse
	buffer            *bufio.ReadWriter
	scanner           *bufio.Scanner
	err               error
	sequenceID        uint64
	writeBuf          *bytes.Buffer
	StatusFlags       *StatusFlags
	Listener          *net.Listener
	packetHeader      *PacketHeader
	scanPos           uint64
}

func newBinlogConn(config *Config) Conn {
	return Conn{
		Config:     config,
		sequenceID: 1,
	}
}

// Prepare is not yet implemented.
func (c Conn) Prepare(query string) (driver.Stmt, error) {
	return nil, nil
}

// Close is not yet implemented.s
func (c Conn) Close() error {
	return nil
}

// Begin is not yet implemented.
func (c Conn) Begin() (driver.Tx, error) {
	return nil, nil
}

// Driver is not used.
type Driver struct{}

// Open creates the connection to the MySQL server.
func (d Driver) Open(dsn string) (driver.Conn, error) {
	config, err := newBinlogConfig(dsn)
	if nil != err {
		return nil, err
	}

	c := newBinlogConn(config)

	var t interface{}
	dialer := net.Dialer{Timeout: c.Config.Timeout}
	addr := fmt.Sprintf("%s:%d", c.Config.Host, c.Config.Port)
	t, err = dialer.Dial("tcp", addr)

	if err != nil {
		netErr, ok := err.(net.Error)
		if ok && !netErr.Temporary() {
			fmt.Printf("Error: %s", netErr.Error())
			return nil, err
		}
	} else {
		c.tcpConn = t.(*net.TCPConn)
		c.setConnection(t.(net.Conn))
	}

	err = c.decodeHandshakePacket()
	if err != nil {
		return nil, err
	}

	c.HandshakeResponse = c.NewHandshakeResponse()

	// If we are on SSL send SSL_Request packet now
	if c.Config.SSL {
		err = c.writeSSLRequestPacket()
		if err != nil {
			return nil, err
		}

		tlsConf := NewClientTLSConfig(
			c.Config.SSLKey,
			c.Config.SSLCer,
			[]byte(c.Config.SSLCA),
			c.Config.VerifyCert,
			c.Config.Host,
		)

		c.secTCPConn = tls.Client(c.tcpConn, tlsConf)
		c.setConnection(c.secTCPConn)
	}

	err = c.writeHandshakeResponse()
	if err != nil {
		return nil, err
	}

	// Listen for auth response.
	_, err = c.readPacket()
	if err != nil {
		return nil, err
	}

	// Auth was successful.
	c.sequenceID = 0

	// Register as a slave
	err = c.registerAsSlave()
	if err != nil {
		return nil, err
	}

	c.sequenceID = 0

	_, err = c.readPacket()
	if err != nil {
		return nil, err
	}

	err = c.startBinlogStream()
	if err != nil {
		return nil, err
	}

	err = c.listenForBinlog()
	if err != nil {
		return nil, err
	}

	return c, err
}

func (c *Conn) readPacket() (interface{}, error) {
	ph, err := c.getPacketHeader()
	if err != nil {
		return nil, err
	}

	var res interface{}

	switch ph.Status {
	case StatusAuth:
		res, err := c.decodeAuthMoreDataResponsePacket(ph)
		if err != nil {
			return nil, err
		}

		switch res.Data {
		case Sha2FastAuthSuccess:
		case Sha2RequestPublicKey:
		case Sha2PerformFullAuthentication:
			c.putBytes(append([]byte(c.Config.Pass), NullByte))
			if c.Flush() != nil {
				return nil, c.Flush()
			}
		}
	case StatusEOF:
		fallthrough
	case StatusOK:
		res, err = c.decodeOKPacket(ph)
		if err != nil {
			return nil, err
		}
	case StatusErr:
		res, err = c.decodeErrorPacket(ph)
		if err != nil {
			return nil, err
		}

		err = fmt.Errorf(
			"error %d: %s",
			res.(*ErrorPacket).ErrorCode,
			res.(*ErrorPacket).ErrorMessage,
		)

		return res, err
	default:
		fmt.Printf("Unknown PacketHeader: %+v\n", ph)
	}

	err = c.scanner.Err()
	if err != nil {
		return nil, err
	}

	return res, nil
}

// PacketHeader represents the beginning of a MySQL protocol packet.
type PacketHeader struct {
	Length     uint64
	SequenceID uint64
	Status     uint64
}

func (c *Conn) getPacketHeader() (*PacketHeader, error) {
	ph := PacketHeader{}
	ph.Length = c.getInt(TypeFixedInt, 3)

	ph.SequenceID = c.getInt(TypeFixedInt, 1)
	ph.Status = c.getInt(TypeFixedInt, 1)
	fmt.Printf("ph.Status = %+v\n", ph.Status)

	err := c.scanner.Err()
	if err != nil {
		return &ph, err
	}

	c.packetHeader = &ph
	c.scanPos = 0

	return &ph, nil
}

func init() {
	sql.Register("mysql-binlog", &Driver{})
}

func (c *Conn) readBytes(l uint64) *bytes.Buffer {
	b := make([]byte, 0)
	for i := uint64(0); i < l; i++ {
		didScan := c.scanner.Scan()
		if !didScan {
			err := c.scanner.Err()
			if err != nil {
				panic(err)
			}
		} else {
			b = append(b, c.scanner.Bytes()...)
		}
	}

	c.scanPos += uint64(len(b))

	return bytes.NewBuffer(b)
}

func (c *Conn) getBytesUntilNull() *bytes.Buffer {
	l := uint64(1)
	s := c.readBytes(l)
	b := s.Bytes()

	for {
		if uint64(s.Len()) != l || s.Bytes()[0] == NullByte {
			break
		}

		s = c.readBytes(l)
		b = append(b, s.Bytes()...)
	}

	return bytes.NewBuffer(b)
}

func (c *Conn) discardBytes(l uint64) {
	c.readBytes(l)
}

func (c *Conn) getInt(t int, l uint64) uint64 {
	var v uint64

	switch t {
	case TypeFixedInt:
		v = c.decFixedInt(l)
	case TypeLenEncInt:
		v = c.decLenEncInt()
	default:
		v = 0
	}

	return v
}

func (c *Conn) getString(t int, l uint64) string {
	var v string

	switch t {
	case TypeFixedString:
		v = c.decFixedString(l)
	case TypeLenEncString:
		v = string(c.decLenEncInt())
	case TypeNullTerminatedString:
		v = c.decNullTerminatedString()
	case TypeRestOfPacketString:
		v = c.decRestOfPacketString()
	default:
		v = ""
	}

	return v
}

func (c *Conn) decRestOfPacketString() string {
	b := c.getRemainingBytes()
	return b.String()
}

func (c *Conn) getRemainingBytes() *bytes.Buffer {
	l := (c.packetHeader.Length - 1) - c.scanPos
	b := c.readBytes(l)

	return b
}

func (c *Conn) decNullTerminatedString() string {
	b := c.getBytesUntilNull()
	return strings.TrimRight(b.String(), string(NullByte))
}

func (c *Conn) decFixedString(l uint64) string {
	b := c.readBytes(l)
	return b.String()
}

func (c *Conn) decLenEncInt() uint64 {
	var l uint16
	b := c.readBytes(1)
	br := bytes.NewReader(b.Bytes())
	_ = binary.Read(br, binary.LittleEndian, &l)
	if l > 0 {
		return c.decFixedInt(uint64(l))
	}

	return 0
}

func (c *Conn) decFixedInt(l uint64) uint64 {
	var i uint64
	b := c.readBytes(l)

	if l <= 2 {
		var x uint16
		pb := c.padBytes(2, b.Bytes())
		br := bytes.NewReader(pb)
		_ = binary.Read(br, binary.LittleEndian, &x)
		i = uint64(x)
	} else if l <= 4 {
		var x uint32
		pb := c.padBytes(4, b.Bytes())
		br := bytes.NewReader(pb)
		_ = binary.Read(br, binary.LittleEndian, &x)
		i = uint64(x)
	} else if l <= 8 {
		var x uint64
		pb := c.padBytes(8, b.Bytes())
		br := bytes.NewReader(pb)
		_ = binary.Read(br, binary.LittleEndian, &x)
		i = x
	}

	return i
}

func (c *Conn) padBytes(l int, b []byte) []byte {
	bl := len(b)
	pl := l - bl
	for i := 0; i < pl; i++ {
		b = append(b, NullByte)
	}

	return b
}

func (c *Conn) encFixedLenInt(v uint64, l uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b[:l]
}

func (c *Conn) encLenEncInt(v uint64) []byte {
	prefix := make([]byte, 1)
	var b []byte
	switch {
	case v < MaxUint08:
		b = make([]byte, 2)
		binary.LittleEndian.PutUint16(b, uint16(v))
		b = b[:1]
	case v >= MaxUint08 && v < MaxUint16:
		prefix[0] = 0xFC
		b = make([]byte, 3)
		binary.LittleEndian.PutUint16(b, uint16(v))
		b = b[:2]
	case v >= MaxUint16 && v < MaxUint24:
		prefix[0] = 0xFD
		b = make([]byte, 4)
		binary.LittleEndian.PutUint32(b, uint32(v))
		b = b[:3]
	case v >= MaxUint24 && v < MaxUint64:
		prefix[0] = 0xFE
		b = make([]byte, 9)
		binary.LittleEndian.PutUint64(b, v)
	}

	if len(b) > 1 {
		b = append(prefix, b...)
	}
	return b
}

func (c *Conn) bitmaskToStruct(b []byte, s interface{}) interface{} {
	l := len(b)
	t := reflect.TypeOf(s)
	v := reflect.New(t.Elem()).Elem()
	for i := uint(0); i < uint(v.NumField()); i++ {
		f := v.Field(int(i))
		var v bool
		switch {
		case l > 4:
			x := binary.LittleEndian.Uint64(b)
			flag := uint64(1 << i)
			v = x&flag > 0
		case l > 2:
			x := binary.LittleEndian.Uint32(b)
			flag := uint32(1 << i)
			v = x&flag > 0
		case l > 1:
			x := binary.LittleEndian.Uint16(b)
			flag := uint16(1 << i)
			v = x&flag > 0
		default:
			x := uint(b[0])
			flag := uint(1 << i)
			v = x&flag > 0
		}

		f.SetBool(v)
	}

	return v.Interface()
}

func (c *Conn) structToBitmask(s interface{}) []byte {
	t := reflect.TypeOf(s).Elem()
	sV := reflect.ValueOf(s).Elem()
	fC := uint(t.NumField())
	m := uint64(0)
	for i := uint(0); i < fC; i++ {
		f := sV.Field(int(i))
		v := f.Bool()
		if v {
			m |= 1 << i
		}
	}

	l := uint64(math.Ceil(float64(fC) / 8.0))
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, m)

	switch {
	case l > 4: // 64 bits
		b = b[:8]
	case l > 2: // 32 bits
		b = b[:4]
	case l > 1: // 16 bits
		b = b[:2]
	default: // 8 bits
		b = b[:1]
	}

	return b
}

func (c *Conn) putString(t int, v string) uint64 {
	b := make([]byte, 0)

	switch t {
	case TypeFixedString:
		b = c.encFixedString(v)
	case TypeLenEncString:
		b = c.encLenEncString(v)
	case TypeNullTerminatedString:
		b = c.encNullTerminatedString(v)
	case TypeRestOfPacketString:
		b = c.encRestOfPacketString(v)
	}

	l, err := c.writeBuf.Write(b)
	if err != nil {
		c.err = err
	}

	return uint64(l)
}

func (c *Conn) encLenEncString(v string) []byte {
	l := uint64(len(v))
	b := c.encLenEncInt(l)
	return append(b, c.encFixedString(v)...)
}

func (c *Conn) encNullTerminatedString(v string) []byte {
	return append([]byte(v), NullByte)
}

func (c *Conn) encFixedString(v string) []byte {
	return []byte(v)
}

func (c *Conn) encRestOfPacketString(v string) []byte {
	s := c.encFixedString(v)
	return s
}

func (c *Conn) putInt(t int, v uint64, l uint64) uint64 {
	c.setupWriteBuffer()

	b := make([]byte, 0)

	switch t {
	case TypeFixedInt:
		b = c.encFixedLenInt(v, l)
	case TypeLenEncInt:
		b = c.encLenEncInt(v)
	}

	n, err := c.writeBuf.Write(b)
	if err != nil {
		c.err = err
	}

	return uint64(n)
}

func (c *Conn) putNullBytes(n uint64) uint64 {
	c.setupWriteBuffer()

	b := make([]byte, n)
	l, err := c.writeBuf.Write(b)
	if err != nil {
		c.err = err
	}

	return uint64(l)
}

func (c *Conn) putBytes(v []byte) uint64 {
	c.setupWriteBuffer()

	l, err := c.writeBuf.Write(v)
	if err != nil {
		c.err = err
	}

	return uint64(l)
}

// Flush clears the write buffer.
func (c *Conn) Flush() error {
	if c.err != nil {
		return c.err
	}

	c.writeBuf = c.addHeader()
	//fmt.Printf("c.writeBuf = %x\n", c.writeBuf.Bytes())
	_, _ = c.buffer.Write(c.writeBuf.Bytes())
	if c.buffer.Flush() != nil {
		return c.buffer.Flush()
	}

	c.writeBuf = nil

	return nil
}

func (c *Conn) addHeader() *bytes.Buffer {
	pl := uint64(c.writeBuf.Len())
	sID := c.sequenceID
	c.sequenceID++

	var plB = c.encFixedLenInt(pl, 3)
	var sIDB = c.encFixedLenInt(sID, 1)
	return bytes.NewBuffer(append(append(plB, sIDB...), c.writeBuf.Bytes()...))
}

func (c *Conn) setupWriteBuffer() {
	if c.writeBuf == nil {
		c.writeBuf = bytes.NewBuffer(nil)
	}
}

// StatusFlags is not used
type StatusFlags struct {
}

// OKPacket represents an OK packet in the MySQL protocol.
type OKPacket struct {
	*PacketHeader
	Header           uint64
	AffectedRows     uint64
	LastInsertID     uint64
	StatusFlags      uint64
	Warnings         uint64
	Info             string
	SessionStateInfo string
}

func (c *Conn) decodeOKPacket(ph *PacketHeader) (*OKPacket, error) {
	op := OKPacket{}
	op.PacketHeader = ph
	op.Header = ph.Status
	op.AffectedRows = c.getInt(TypeLenEncInt, 0)
	op.LastInsertID = c.getInt(TypeLenEncInt, 0)
	if c.HandshakeResponse.ClientFlag.Protocol41 {
		op.StatusFlags = c.getInt(TypeFixedInt, 2)
		op.Warnings = c.getInt(TypeFixedInt, 1)
	} else if c.HandshakeResponse.ClientFlag.Transactions {
		op.StatusFlags = c.getInt(TypeFixedInt, 2)
	}

	if c.HandshakeResponse.ClientFlag.SessionTrack {
		op.Info = c.getString(TypeRestOfPacketString, 0)
	} else {
		op.Info = c.getString(TypeRestOfPacketString, 0)
	}

	return &op, nil
}

// ErrorPacket represents an error packet in the MySQL protocol.
type ErrorPacket struct {
	*PacketHeader
	ErrorCode      uint64
	ErrorMessage   string
	SQLStateMarker string
	SQLState       string
}

func (c *Conn) decodeErrorPacket(ph *PacketHeader) (*ErrorPacket, error) {
	ep := ErrorPacket{}
	ep.PacketHeader = ph
	ep.ErrorCode = c.getInt(TypeFixedInt, 2)
	ep.SQLStateMarker = c.getString(TypeFixedString, 1)
	ep.SQLState = c.getString(TypeFixedString, 5)
	ep.ErrorMessage = c.getString(TypeRestOfPacketString, 0)

	err := c.scanner.Err()
	if err != nil {
		return nil, err
	}

	return &ep, nil
}

func (c *Conn) setConnection(nc net.Conn) {
	c.curConn = nc

	c.buffer = bufio.NewReadWriter(
		bufio.NewReader(c.curConn),
		bufio.NewWriter(c.curConn),
	)

	c.scanner = bufio.NewScanner(c.buffer.Reader)
	c.scanner.Split(bufio.ScanBytes)
}
