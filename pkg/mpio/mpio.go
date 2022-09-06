// Copyright 2022 Dashborg Inc
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package mpio

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/scripthaus-dev/mshell/pkg/base"
	"github.com/scripthaus-dev/mshell/pkg/packet"
)

const ReadBufSize = 128 * 1024
const WriteBufSize = 128 * 1024
const MaxSingleWriteSize = 4 * 1024
const MaxTotalRunDataSize = 10 * ReadBufSize

type Multiplexer struct {
	Lock            *sync.Mutex
	CK              base.CommandKey
	FdReaders       map[int]*FdReader // synchronized
	FdWriters       map[int]*FdWriter // synchronized
	RunData         map[int]*FdReader // synchronized
	CloseAfterStart []*os.File        // synchronized
	PtyFd           *os.File
	CmdProc         *os.Process

	Sender  *packet.PacketSender
	Input   *packet.PacketParser
	Started bool
	UPR     packet.UnknownPacketReporter

	Debug bool
}

func MakeMultiplexer(ck base.CommandKey, upr packet.UnknownPacketReporter) *Multiplexer {
	if upr == nil {
		upr = packet.DefaultUPR{}
	}
	return &Multiplexer{
		Lock:      &sync.Mutex{},
		CK:        ck,
		FdReaders: make(map[int]*FdReader),
		FdWriters: make(map[int]*FdWriter),
		UPR:       upr,
	}
}

func (m *Multiplexer) SetPtyFd(ptyFd *os.File) {
	m.Lock.Lock()
	defer m.Lock.Unlock()
	m.PtyFd = ptyFd
}

func (m *Multiplexer) Close() {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	for _, fr := range m.FdReaders {
		fr.Close()
	}
	for _, fw := range m.FdWriters {
		fw.Close()
	}
	for _, fd := range m.CloseAfterStart {
		fd.Close()
	}
}

func (m *Multiplexer) HandleInputDone() {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	// close readers (obviously the done command needs no more input)
	for _, fr := range m.FdReaders {
		fr.Close()
	}

	// ensure EOF on all writers (ignore error)
	for _, fw := range m.FdWriters {
		fw.AddData(nil, true)
	}
}

// returns the *writer* to connect to process, reader is put in FdReaders
func (m *Multiplexer) MakeReaderPipe(fdNum int) (*os.File, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	m.Lock.Lock()
	defer m.Lock.Unlock()
	m.FdReaders[fdNum] = MakeFdReader(m, pr, fdNum, true, false)
	m.CloseAfterStart = append(m.CloseAfterStart, pw)
	return pw, nil
}

// returns the *reader* to connect to process, writer is put in FdWriters
func (m *Multiplexer) MakeWriterPipe(fdNum int) (*os.File, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	m.Lock.Lock()
	defer m.Lock.Unlock()
	m.FdWriters[fdNum] = MakeFdWriter(m, pw, fdNum, true)
	m.CloseAfterStart = append(m.CloseAfterStart, pr)
	return pr, nil
}

// returns the *reader* to connect to process, writer is put in FdWriters
func (m *Multiplexer) MakeStaticWriterPipe(fdNum int, data []byte) (*os.File, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	m.Lock.Lock()
	defer m.Lock.Unlock()
	fdWriter := MakeFdWriter(m, pw, fdNum, true)
	err = fdWriter.AddData(data, true)
	if err != nil {
		return nil, err
	}
	m.FdWriters[fdNum] = fdWriter
	m.CloseAfterStart = append(m.CloseAfterStart, pr)
	return pr, nil
}

func (m *Multiplexer) MakeRawFdReader(fdNum int, fd io.ReadCloser, shouldClose bool, isPty bool) {
	m.Lock.Lock()
	defer m.Lock.Unlock()
	m.FdReaders[fdNum] = MakeFdReader(m, fd, fdNum, shouldClose, isPty)
}

func (m *Multiplexer) MakeRawFdWriter(fdNum int, fd io.WriteCloser, shouldClose bool) {
	m.Lock.Lock()
	defer m.Lock.Unlock()
	m.FdWriters[fdNum] = MakeFdWriter(m, fd, fdNum, shouldClose)
}

func (m *Multiplexer) makeDataAckPacket(fdNum int, ackLen int, err error) *packet.DataAckPacketType {
	ack := packet.MakeDataAckPacket()
	ack.CK = m.CK
	ack.FdNum = fdNum
	ack.AckLen = ackLen
	if err != nil {
		ack.Error = err.Error()
	}
	return ack
}

func (m *Multiplexer) makeDataPacket(fdNum int, data []byte, err error) *packet.DataPacketType {
	pk := packet.MakeDataPacket()
	pk.CK = m.CK
	pk.FdNum = fdNum
	pk.Data64 = base64.StdEncoding.EncodeToString(data)
	if err != nil {
		pk.Error = err.Error()
	}
	return pk
}

func (m *Multiplexer) sendPacket(p packet.PacketType) {
	m.Sender.SendPacket(p)
}

func (m *Multiplexer) launchWriters(wg *sync.WaitGroup) {
	m.Lock.Lock()
	defer m.Lock.Unlock()
	if wg != nil {
		wg.Add(len(m.FdWriters))
	}
	for _, fw := range m.FdWriters {
		go fw.WriteLoop(wg)
	}
}

func (m *Multiplexer) launchReaders(wg *sync.WaitGroup) {
	m.Lock.Lock()
	defer m.Lock.Unlock()
	if wg != nil {
		wg.Add(len(m.FdReaders))
	}
	for _, fr := range m.FdReaders {
		go fr.ReadLoop(wg)
	}
}

func (m *Multiplexer) startIO(packetParser *packet.PacketParser, sender *packet.PacketSender) {
	m.Lock.Lock()
	defer m.Lock.Unlock()
	if m.Started {
		panic("Multiplexer is already running, cannot start again")
	}
	m.Input = packetParser
	m.Sender = sender
	m.Started = true
}

func (m *Multiplexer) runPacketInputLoop() *packet.CmdDonePacketType {
	defer m.HandleInputDone()
	for pk := range m.Input.MainCh {
		if m.Debug {
			fmt.Printf("PK-M> %s\n", packet.AsString(pk))
		}
		if pk.GetType() == packet.DataPacketStr {
			dataPacket := pk.(*packet.DataPacketType)
			err := m.processDataPacket(dataPacket)
			if err != nil {
				errPacket := m.makeDataAckPacket(dataPacket.FdNum, 0, err)
				m.sendPacket(errPacket)
			}
			continue
		}
		if pk.GetType() == packet.DataAckPacketStr {
			ackPacket := pk.(*packet.DataAckPacketType)
			m.processAckPacket(ackPacket)
			continue
		}
		if pk.GetType() == packet.CmdDonePacketStr {
			donePacket := pk.(*packet.CmdDonePacketType)
			return donePacket
		}
		if pk.GetType() == packet.SpecialInputPacketStr {
			inputPacket := pk.(*packet.SpecialInputPacketType)
			m.processSpecialInputPacket(inputPacket)
		}
		m.UPR.UnknownPacket(pk)
	}
	return nil
}

func (m *Multiplexer) processSpecialInputPacket(pk *packet.SpecialInputPacketType) {
	m.Lock.Lock()
	ptyFd := m.PtyFd
	cmdProc := m.CmdProc
	m.Lock.Unlock()
	if ptyFd == nil {
		// no pty, maybe send a message back to server, but the server always starts with a pty, so this shouldn't be an issue
		return
	}
	if pk.WinSize != nil {
		winSize := &pty.Winsize{
			//Rows: base.BoundInt(pk.WinSize.Rows, shexec.MinTermRows, shexec.MaxTermRows),
			//Cols: base.BoundInt(pk.Winsize.Cols, shexec.MinTermCols, shexec.MaxTermCols),
		}
		pty.Setsize(ptyFd, winSize)
		if cmdProc != nil {
			cmdProc.Signal(syscall.SIGWINCH)
		}
	}
}

func (m *Multiplexer) processDataPacket(dataPacket *packet.DataPacketType) error {
	realData, err := base64.StdEncoding.DecodeString(dataPacket.Data64)
	if err != nil {
		return fmt.Errorf("decoding base64 data: %w", err)
	}
	m.Lock.Lock()
	defer m.Lock.Unlock()
	fw := m.FdWriters[dataPacket.FdNum]
	if fw == nil {
		// add a closed FdWriter as a placeholder so we only send one error
		fw := MakeFdWriter(m, nil, dataPacket.FdNum, false)
		fw.Close()
		m.FdWriters[dataPacket.FdNum] = fw
		return fmt.Errorf("write to closed file (no fd)")
	}
	err = fw.AddData(realData, dataPacket.Eof)
	if err != nil {
		fw.Close()
		return err
	}
	return nil
}

func (m *Multiplexer) processAckPacket(ackPacket *packet.DataAckPacketType) {
	m.Lock.Lock()
	defer m.Lock.Unlock()
	fr := m.FdReaders[ackPacket.FdNum]
	if fr == nil {
		return
	}
	fr.NotifyAck(ackPacket.AckLen)
}

func (m *Multiplexer) closeTempStartFds() {
	m.Lock.Lock()
	defer m.Lock.Unlock()
	for _, fd := range m.CloseAfterStart {
		fd.Close()
	}
	m.CloseAfterStart = nil
}

func (m *Multiplexer) RunIOAndWait(packetParser *packet.PacketParser, sender *packet.PacketSender, waitOnReaders bool, waitOnWriters bool, waitForInputLoop bool) *packet.CmdDonePacketType {
	m.startIO(packetParser, sender)
	m.closeTempStartFds()
	var wg sync.WaitGroup
	if waitOnReaders {
		m.launchReaders(&wg)
	} else {
		m.launchReaders(nil)
	}
	if waitOnWriters {
		m.launchWriters(&wg)
	} else {
		m.launchWriters(nil)
	}
	var donePacket *packet.CmdDonePacketType
	if waitForInputLoop {
		wg.Add(1)
	}
	go func() {
		if waitForInputLoop {
			defer wg.Done()
		}
		pkRtn := m.runPacketInputLoop()
		if pkRtn != nil {
			m.Lock.Lock()
			donePacket = pkRtn
			m.Lock.Unlock()
		}
	}()
	wg.Wait()

	m.Lock.Lock()
	defer m.Lock.Unlock()
	return donePacket
}
