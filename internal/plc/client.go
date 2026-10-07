package plc

import (
	"errors"
	"fmt"
	"time"

	"github.com/goburrow/modbus"
)

// Client is one Modbus TCP connection to one PLC.
// Not safe for concurrent use — each PLC worker owns exactly one Client.
type Client struct {
	handler   *modbus.TCPClientHandler
	client    modbus.Client
	connected bool
}

func NewClient(ip string, port, unitID int, timeout time.Duration) *Client {
	h := modbus.NewTCPClientHandler(fmt.Sprintf("%s:%d", ip, port))
	h.SlaveId = byte(unitID)
	h.Timeout = timeout
	// Keep the socket open between scans (goburrow closes it after
	// IdleTimeout without traffic). Scans are far more frequent than this.
	h.IdleTimeout = 5 * time.Minute
	return &Client{handler: h, client: modbus.NewClient(h)}
}

func (c *Client) Connect() error {
	if err := c.handler.Connect(); err != nil {
		return err
	}
	c.connected = true
	return nil
}

func (c *Client) Connected() bool { return c.connected }

func (c *Client) Close() {
	c.handler.Close()
	c.connected = false
}

// ReadBlock returns the points of one block as uint16 values
// (0/1 for discrete inputs, the 16-bit word for registers).
func (c *Client) ReadBlock(b Block) ([]uint16, error) {
	var (
		data []byte
		err  error
	)
	switch b.Kind {
	case DiscreteInput:
		data, err = c.client.ReadDiscreteInputs(b.Start, b.Count)
		if err != nil {
			return nil, err
		}
		return unpackBits(data, int(b.Count))
	case HoldingRegister:
		data, err = c.client.ReadHoldingRegisters(b.Start, b.Count)
	default:
		data, err = c.client.ReadInputRegisters(b.Start, b.Count)
	}
	if err != nil {
		return nil, err
	}
	if len(data) != int(b.Count)*2 {
		return nil, fmt.Errorf("expected %d bytes, got %d", b.Count*2, len(data))
	}
	values := make([]uint16, b.Count)
	for i := range values {
		values[i] = uint16(data[2*i])<<8 | uint16(data[2*i+1]) // Modbus is big-endian
	}
	return values, nil
}

// IsModbusException reports whether the PLC answered with a Modbus
// exception (e.g. illegal data address). The connection is still fine
// in that case; any other error means the connection should be reset.
func IsModbusException(err error) bool {
	var me *modbus.ModbusError
	return errors.As(err, &me)
}

// unpackBits expands a Modbus bit response: 8 inputs per byte,
// first input in the least significant bit of the first byte.
func unpackBits(data []byte, count int) ([]uint16, error) {
	if len(data) != (count+7)/8 {
		return nil, fmt.Errorf("expected %d bytes for %d bits, got %d", (count+7)/8, count, len(data))
	}
	values := make([]uint16, count)
	for i := range values {
		values[i] = uint16(data[i/8]>>(i%8)) & 1
	}
	return values, nil
}
