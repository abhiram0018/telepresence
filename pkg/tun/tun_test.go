package tun

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/signal"
	"sync"
	"testing"

	"github.com/stretchr/testify/suite"
	"golang.org/x/sys/unix"

	"github.com/datawire/ambassador/pkg/dtest"
	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/v2/pkg/subnet"
)

func TestTun(t *testing.T) {
	dtest.Sudo()
	dtest.WithMachineLock(func() {
		suite.Run(t, new(tunSuite))
	})
}

type tunSuite struct {
	suite.Suite
	ctx context.Context
	tun *Device
}

func (ts *tunSuite) SetupSuite() {
	t := ts.T()
	ctx := dlog.WithLogger(context.Background(), dlog.WrapTB(t, false))
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	t.Cleanup(cancel)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, unix.SIGINT, unix.SIGTERM, unix.SIGQUIT, unix.SIGABRT, unix.SIGHUP)
	go func() {
		<-sigCh
		cancel()
	}()
	ts.ctx = ctx

	require := ts.Require()
	tun, err := OpenTun()
	require.NoError(err, "Failed to open TUN device")
	ts.tun = tun
}

func reader(tun *Device) (<-chan []byte, <-chan error) {
	buf := make([]byte, 0x10000)
	dataCh := make(chan []byte)
	errCh := make(chan error)
	go func() {
		for {
			n, err := tun.Read(buf)
			if err != nil {
				errCh <- err
			} else {
				dataCh <- buf[:n]
			}
		}
	}()
	return dataCh, errCh
}

func writer(c context.Context, tun *Device, dataChan <-chan []byte) {
	go func() {
		for {
			select {
			case buf := <-dataChan:
				_, err := tun.Write(buf)
				if err != nil {
					if c.Err() != nil {
						err = nil
					}
					return
				}
			case <-c.Done():
				return
			}
		}
	}()
}

func (ts *tunSuite) TestTunnel() {
	require := ts.Require()
	addr, err := subnet.FindAvailableClassC()
	require.NoError(err)

	to := make(net.IP, 4)
	copy(to, addr.IP)
	to[3] = 1

	testIP := make(net.IP, 4)
	copy(testIP, addr.IP)
	testIP[3] = 123

	testData := []byte("some stuff")
	testDataReceived := false

	require.NoError(ts.tun.AddSubnet(ts.ctx, addr, to))

	dataChan, errChan := reader(ts.tun)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case buf := <-dataChan:
				// Skip everything but ipv4 UDP requests to dnsIP port 53
				h, err := ParseIPv4Header(buf)
				require.NoError(err)
				if !h.Dst.Equal(testIP) {
					dlog.Info(ts.ctx, h)
					continue
				}
				buf = buf[:h.TotalLen]
				if h.Protocol == udpProto {
					// We've got an UDP package to our test destination
					dg := udpDatagram(buf[h.Len:])
					if dg.destination() == 8080 {
						testDataReceived = bytes.Equal(testData, dg.body())
						return
					}
				}
			case <-ts.ctx.Done():
				dlog.Info(ts.ctx, "context cancelled")
				return
			case err = <-errChan:
				if ts.ctx.Err() == nil {
					require.NoError(err)
				}
				return
			}
		}
	}()

	conn, err := net.Dial("udp", testIP.String()+":8080")
	require.NoError(err)
	defer conn.Close()
	_, err = conn.Write(testData)
	require.NoError(err)
	wg.Wait()
	require.True(testDataReceived)
}

func (ts *tunSuite) TestFakeReader() {
	require := ts.Require()
	addr, err := subnet.FindAvailableClassC()
	require.NoError(err)

	to := make(net.IP, 4)
	copy(to, addr.IP)
	to[3] = 1

	testIP := make(net.IP, 4)
	copy(testIP, addr.IP)
	testIP[3] = 123

	testData := []byte("some stuff")
	testDataReceived := false

	require.NoError(ts.tun.AddSubnet(ts.ctx, addr, to))

	dataChan, errChan := reader(ts.tun)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case buf := <-dataChan:
				// Skip everything but ipv4 UDP requests to dnsIP port 53
				h, err := ParseIPv4Header(buf)
				require.NoError(err)
				buf = buf[:h.TotalLen]
				if h.Dst.Equal(testIP) && h.Protocol == udpProto {
					// We've got an UDP package to our test destination
					dg := udpDatagram(buf[h.Len:])
					if dg.destination() == 8080 {
						testDataReceived = bytes.Equal(testData, dg.body())
						return
					}
				}
				dlog.Info(ts.ctx, h)
			case <-ts.ctx.Done():
				dlog.Info(ts.ctx, "context cancelled")
				return
			case err = <-errChan:
				if ts.ctx.Err() == nil {
					require.NoError(err)
				}
				return
			}
		}
	}()

	conn, err := net.Dial("udp", testIP.String()+":8080")
	require.NoError(err)
	defer conn.Close()
	_, err = conn.Write(testData)
	require.NoError(err)
	wg.Wait()
	require.True(testDataReceived)
}

func (ts *tunSuite) TearDownSuite() {
	if ts.tun != nil {
		ts.tun.Close()
	}
}
