// Copyright 2018 The gVisor Authors.
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

package ptrace

import (
	"fmt"
	"os"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/usermem"
)

// Linux kernel errnos which "should never be seen by user programs", but will
// be revealed to ptrace syscall exit tracing.
//
// These constants are only used in subprocess.go.
const (
	ERESTARTSYS    = syscall.Errno(512)
	ERESTARTNOINTR = syscall.Errno(513)
	ERESTARTNOHAND = syscall.Errno(514)
)

// globalPool exists to solve two distinct problems:
//
// 1) Subprocesses can't always be killed properly (see Release).
//
// 2) Any seccomp filters that have been installed will apply to subprocesses
// created here. Therefore we use the intermediary (master), which is created
// on initialization of the platform.
var globalPool struct {
	mu        sync.Mutex
	master    *subprocess
	available []*subprocess
}

// thread is a traced thread; it is a thread identifier.
//
// This is a convenience type for defining ptrace operations.
type thread struct {
	tgid int32
	tid  int32
	cpu  uint32

	// initRegs are the initial registers for the first thread.
	//
	// These are used for the register set for system calls.
	initRegs arch.Registers
}

type createStubRPCRet struct {
	child *thread
	err   error
}

type createStubRPC struct {
	ret chan createStubRPCRet
}

// subprocess is a collection of threads being traced.
type subprocess struct {
	platform.NoAddressSpaceIO

	// requests is used to signal creation of new threads.
	requests chan chan *thread

	createStubRPCChan chan createStubRPC

	syscallRPCChan chan syscallRPC

	// mu protects the following fields.
	mu sync.Mutex

	sysemuRPCPool map[int64]chan sysemuRPC

	// contexts is the set of contexts for which it's possible that
	// context.lastFaultSP == this subprocess.
	contexts map[*context]struct{}
}

// newSubprocess returns a usable subprocess.
//
// This will either be a newly created subprocess, or one from the global pool.
// The create function will be called in the latter case, which is guaranteed
// to happen with the runtime thread locked.
func newSubprocess(create func() (*thread, error)) (*subprocess, error) {
	// See Release.
	globalPool.mu.Lock()
	if len(globalPool.available) > 0 {
		sp := globalPool.available[len(globalPool.available)-1]
		globalPool.available = globalPool.available[:len(globalPool.available)-1]
		globalPool.mu.Unlock()
		return sp, nil
	}
	globalPool.mu.Unlock()

	// The following goroutine is responsible for creating the first traced
	// thread, and responding to requests to make additional threads in the
	// traced process. The process will be killed and reaped when the
	// request channel is closed, which happens in Release below.
	errChan := make(chan error)
	requests := make(chan chan *thread)
	go func() { // S/R-SAFE: Platform-related.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		// Initialize the first thread.
		firstThread, err := create()
		if err != nil {
			errChan <- err
			return
		}
		firstThread.grabInitRegs()

		// Ready to handle requests.
		errChan <- nil

		// Wait for requests to create threads.
		for r := range requests {
			t, err := firstThread.clone()
			if err != nil {
				// Should not happen: not recoverable.
				panic(fmt.Sprintf("error initializing first thread: %v", err))
			}

			// Since the new thread was created with
			// clone(CLONE_PTRACE), it will begin execution with
			// SIGSTOP pending and with this thread as its tracer.
			// (Hopefully nobody tgkilled it with a signal <
			// SIGSTOP before the SIGSTOP was delivered, in which
			// case that signal would be delivered before SIGSTOP.)
			if sig := t.wait(stopped); sig != syscall.SIGSTOP {
				panic(fmt.Sprintf("error waiting for new clone: expected SIGSTOP, got %v", sig))
			}

			// Detach the thread.
			t.detach()
			t.initRegs = firstThread.initRegs

			// Return the thread.
			r <- t
		}

		// Requests should never be closed.
		panic("unreachable")
	}()

	// Wait until error or readiness.
	if err := <-errChan; err != nil {
		return nil, err
	}

	// Ready.
	sp := &subprocess{
		requests:          requests,
		sysemuRPCPool:     make(map[int64]chan sysemuRPC),
		syscallRPCChan:    make(chan syscallRPC),
		createStubRPCChan: make(chan createStubRPC),
		contexts:          make(map[*context]struct{}),
	}
	go sp.syscallLoop(sp.syscallRPCChan)
	go sp.createStubLoop(sp.createStubRPCChan)

	sp.unmap()
	return sp, nil
}

// unmap unmaps non-stub regions of the process.
//
// This will panic on failure (which should never happen).
func (s *subprocess) unmap() {
	s.Unmap(0, uint64(stubStart))
	if maximumUserAddress != stubEnd {
		s.Unmap(usermem.Addr(stubEnd), uint64(maximumUserAddress-stubEnd))
	}
}

// Release kills the subprocess.
//
// Just kidding! We can't safely co-ordinate the detaching of all the
// tracees (since the tracers are random runtime threads, and the process
// won't exit until tracers have been notifier).
//
// Therefore we simply unmap everything in the subprocess and return it to the
// globalPool. This has the added benefit of reducing creation time for new
// subprocesses.
func (s *subprocess) Release() {
	go func() { // S/R-SAFE: Platform.
		s.unmap()
		globalPool.mu.Lock()
		globalPool.available = append(globalPool.available, s)
		globalPool.mu.Unlock()
	}()
}

// newThread creates a new traced thread.
//
// Precondition: the OS thread must be locked.
func (s *subprocess) newThread() *thread {
	// Ask the first thread to create a new one.
	r := make(chan *thread)
	s.requests <- r
	t := <-r
	close(r)

	// Attach the subprocess to this one.
	t.attach()

	// Return the new thread, which is now bound.
	return t
}

// attach attaches to the thread.
func (t *thread) attach() {
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE, syscall.PTRACE_ATTACH, uintptr(t.tid), 0, 0, 0, 0); errno != 0 {
		panic(fmt.Sprintf("unable to attach: %v", errno))
	}

	// PTRACE_ATTACH sends SIGSTOP, and wakes the tracee if it was already
	// stopped from the SIGSTOP queued by CLONE_PTRACE (see inner loop of
	// newSubprocess), so we always expect to see signal-delivery-stop with
	// SIGSTOP.
	if sig := t.wait(stopped); sig != syscall.SIGSTOP {
		panic(fmt.Sprintf("wait failed: expected SIGSTOP, got %v", sig))
	}

	// Initialize options.
	t.init()
}

func (t *thread) grabInitRegs() {
	// Grab registers.
	//
	// Note that we adjust the current register RIP value to be just before
	// the current system call executed. This depends on the definition of
	// the stub itself.
	if err := t.getRegs(&t.initRegs); err != nil {
		panic(fmt.Sprintf("ptrace get regs failed: %v", err))
	}
	t.adjustInitRegsRip()
}

// detach detaches from the thread.
//
// Because the SIGSTOP is not suppressed, the thread will enter group-stop.
func (t *thread) detach() {
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE, syscall.PTRACE_DETACH, uintptr(t.tid), 0, uintptr(syscall.SIGSTOP), 0, 0); errno != 0 {
		panic(fmt.Sprintf("can't detach new clone: %v", errno))
	}
}

// waitOutcome is used for wait below.
type waitOutcome int

const (
	// stopped indicates that the process was stopped.
	stopped waitOutcome = iota

	// killed indicates that the process was killed.
	killed

	exited
)

func (t *thread) dumpAndPanic(message string) {
	var regs arch.Registers
	message += "\n"
	if err := t.getRegs(&regs); err == nil {
		message += dumpRegs(&regs)
	} else {
		log.Warningf("unable to get registers: %v", err)
	}
	message += fmt.Sprintf("stubStart\t = %016x\n", stubStart)
	panic(message)
}

func (t *thread) unexpectedStubExit() {
	msg, err := t.getEventMessage()
	status := syscall.WaitStatus(msg)
	if status.Signaled() && status.Signal() == syscall.SIGKILL {
		// SIGKILL can be only sent by a user or OOM-killer. In both
		// these cases, we don't need to panic. There is no reasons to
		// think that something wrong in gVisor.
		log.Warningf("The ptrace stub process %v has been killed by SIGKILL.", t.tgid)
		pid := os.Getpid()
		syscall.Tgkill(pid, pid, syscall.Signal(syscall.SIGKILL))
	}
	t.dumpAndPanic(fmt.Sprintf("wait failed: the process %d:%d exited: %x (err %v)", t.tgid, t.tid, msg, err))
}

// wait waits for a stop event.
//
// Precondition: outcome is a valid waitOutcome.
func (t *thread) wait(outcome waitOutcome) syscall.Signal {
	var status syscall.WaitStatus

	for {
		r, err := syscall.Wait4(int(t.tid), &status, syscall.WALL|syscall.WUNTRACED, nil)
		if err == syscall.EINTR || err == syscall.EAGAIN {
			// Wait was interrupted; wait again.
			continue
		} else if err != nil {
			panic(fmt.Sprintf("ptrace wait failed: %v", err))
		}
		if int(r) != int(t.tid) {
			panic(fmt.Sprintf("ptrace wait returned %v, expected %v", r, t.tid))
		}
		switch outcome {
		case stopped:
			if !status.Stopped() {
				t.dumpAndPanic(fmt.Sprintf("ptrace status unexpected: got %v, wanted stopped", status))
			}
			stopSig := status.StopSignal()
			if stopSig == 0 {
				continue // Spurious stop.
			}
			if stopSig == syscall.SIGTRAP {
				if status.TrapCause() == syscall.PTRACE_EVENT_EXIT {
					t.unexpectedStubExit()
				}
				// Re-encode the trap cause the way it's expected.
				return stopSig | syscall.Signal(status.TrapCause()<<8)
			}
			// Not a trap signal.
			return stopSig
		case killed:
			if !status.Exited() && !status.Signaled() {
				t.dumpAndPanic(fmt.Sprintf("ptrace status unexpected: got %v, wanted exited", status))
			}
			return syscall.Signal(status.ExitStatus())
		case exited:
			if !status.Stopped() {
				t.dumpAndPanic(fmt.Sprintf("ptrace status unexpected: got %v, wanted stopped", status))
			}
			stopSig := status.StopSignal()
			if stopSig == 0 {
				continue // Spurious stop.
			}
			if stopSig == syscall.SIGTRAP {
				if status.TrapCause() == syscall.PTRACE_EVENT_EXIT {
					// Re-encode the trap cause the way it's expected.
					return stopSig | syscall.Signal(status.TrapCause()<<8)
				}
				t.dumpAndPanic(fmt.Sprintf("unexpected cause: %v", status.TrapCause()))
			}
			// Not a trap signal.
			return stopSig
		default:
			// Should not happen.
			t.dumpAndPanic(fmt.Sprintf("unknown outcome: %v", outcome))
		}
	}
}

// destroy kills the thread.
//
// Note that this should not be used in the general case; the death of threads
// will typically cause the death of the parent. This is a utility method for
// manually created threads.
func (t *thread) destroy() {
	t.detach()
	syscall.Tgkill(int(t.tgid), int(t.tid), syscall.Signal(syscall.SIGKILL))
	t.wait(killed)
}

// exit stops the thread.
func (t *thread) exit() {
	t.syscallIgnoreInterruptWithOutcome(&t.initRegs, syscall.SYS_EXIT, exited, arch.SyscallArgument{Value: 0})
	t.detach()
}

// init initializes trace options.
func (t *thread) init() {
	// Set the TRACESYSGOOD option to differentiate real SIGTRAP.
	// set PTRACE_O_EXITKILL to ensure that the unexpected exit of the
	// sentry will immediately kill the associated stubs.
	const PTRACE_O_EXITKILL = 0x100000
	_, _, errno := syscall.RawSyscall6(
		syscall.SYS_PTRACE,
		syscall.PTRACE_SETOPTIONS,
		uintptr(t.tid),
		0,
		syscall.PTRACE_O_TRACESYSGOOD|syscall.PTRACE_O_TRACEEXIT|PTRACE_O_EXITKILL,
		0, 0)
	if errno != 0 {
		panic(fmt.Sprintf("ptrace set options failed: %v", errno))
	}
}

// syscall executes a system call cycle in the traced context.
//
// This is _not_ for use by application system calls, rather it is for use when
// a system call must be injected into the remote context (e.g. mmap, munmap).
// Note that clones are handled separately.
func (t *thread) syscall(regs *arch.Registers) (uintptr, error) {
	// Set registers.
	if err := t.setRegs(regs); err != nil {
		panic(fmt.Sprintf("ptrace set regs failed: %v", err))
	}

	for {
		// Execute the syscall instruction. The task has to stop on the
		// trap instruction which is right after the syscall
		// instruction.
		if _, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE, syscall.PTRACE_CONT, uintptr(t.tid), 0, 0, 0, 0); errno != 0 {
			panic(fmt.Sprintf("ptrace syscall-enter failed: %v", errno))
		}

		sig := t.wait(stopped)
		if sig == syscall.SIGTRAP {
			// Reached syscall-enter-stop.
			break
		} else {
			// Some other signal caused a thread stop; ignore.
			if sig != syscall.SIGSTOP && sig != syscall.SIGCHLD {
				log.Warningf("The thread %d:%d has been interrupted by %d", t.tgid, t.tid, sig)
			}
			continue
		}
	}

	// Grab registers.
	if err := t.getRegs(regs); err != nil {
		panic(fmt.Sprintf("ptrace get regs failed: %v", err))
	}

	return syscallReturnValue(regs)
}

func (t *thread) syscallWithOutcome(regs *arch.Registers, outcome waitOutcome) (uintptr, error) {
	// Set registers.
	if err := t.setRegs(regs); err != nil {
		panic(fmt.Sprintf("ptrace set regs failed: %v", err))
	}

	for {
		// Execute the syscall instruction. The task has to stop on the
		// trap instruction which is right after the syscall
		// instruction.
		if _, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE, syscall.PTRACE_CONT, uintptr(t.tid), 0, 0, 0, 0); errno != 0 {
			panic(fmt.Sprintf("ptrace syscall-enter failed: %v", errno))
		}

		sig := t.wait(outcome)
		if (sig & syscall.SIGTRAP) != 0 {
			// Reached syscall-enter-stop.
			break
		} else {
			// Some other signal caused a thread stop; ignore.
			if sig != syscall.SIGSTOP && sig != syscall.SIGCHLD {
				log.Warningf("The thread %d:%d has been interrupted by %d", t.tgid, t.tid, sig)
			}
			continue
		}
	}

	// Grab registers.
	if err := t.getRegs(regs); err != nil {
		panic(fmt.Sprintf("ptrace get regs failed: %v", err))
	}

	return syscallReturnValue(regs)
}

// syscallIgnoreInterrupt ignores interrupts on the system call thread and
// restarts the syscall if the kernel indicates that should happen.
func (t *thread) syscallIgnoreInterrupt(
	initRegs *arch.Registers,
	sysno uintptr,
	args ...arch.SyscallArgument) (uintptr, error) {
	for {
		regs := createSyscallRegs(initRegs, sysno, args...)
		rval, err := t.syscall(&regs)
		switch err {
		case ERESTARTSYS:
			continue
		case ERESTARTNOINTR:
			continue
		case ERESTARTNOHAND:
			continue
		default:
			return rval, err
		}
	}
}

func (t *thread) syscallIgnoreInterruptWithOutcome(
	initRegs *arch.Registers,
	sysno uintptr,
	outcome waitOutcome,
	args ...arch.SyscallArgument) (uintptr, error) {
	for {
		regs := createSyscallRegs(initRegs, sysno, args...)
		rval, err := t.syscallWithOutcome(&regs, outcome)
		switch err {
		case ERESTARTSYS:
			continue
		case ERESTARTNOINTR:
			continue
		case ERESTARTNOHAND:
			continue
		default:
			return rval, err
		}
	}
}

// NotifyInterrupt implements interrupt.Receiver.NotifyInterrupt.
func (t *thread) NotifyInterrupt() {
	syscall.Tgkill(int(t.tgid), int(t.tid), syscall.Signal(platform.SignalInterrupt))
}

type sysemuRPC struct {
	c   *context
	ac  arch.Context
	ret chan bool
}

func (s *subprocess) switchToAppLocked(t *thread, c *context, ac arch.Context) bool {
	// Extract floating point state.
	fpState := ac.FloatingPointData()
	fpLen, _ := ac.FeatureSet().ExtendedStateSize()
	useXsave := ac.FeatureSet().UseXsave()

	// Reset necessary registers.
	regs := &ac.StateData().Regs
	t.resetSysemuRegs(regs)

	// Extract TLS register
	tls := uint64(ac.TLS())

	// Check for interrupts, and ensure that future interrupts will signal t.
	if !c.interrupt.Enable(t) {
		// Pending interrupt; simulate.
		c.signalInfo = arch.SignalInfo{Signo: int32(platform.SignalInterrupt)}
		return false
	}
	defer c.interrupt.Disable()

	// Set registers.
	if err := t.setRegs(regs); err != nil {
		panic(fmt.Sprintf("ptrace set regs (%+v) failed: %v", regs, err))
	}
	if err := t.setFPRegs(fpState, uint64(fpLen), useXsave); err != nil {
		panic(fmt.Sprintf("ptrace set fpregs (%+v) failed: %v", fpState, err))
	}
	if err := t.setTLS(&tls); err != nil {
		panic(fmt.Sprintf("ptrace set tls (%+v) failed: %v", tls, err))
	}

	for {
		// Start running until the next system call.
		if isSingleStepping(regs) {
			if _, _, errno := syscall.RawSyscall6(
				syscall.SYS_PTRACE,
				unix.PTRACE_SYSEMU_SINGLESTEP,
				uintptr(t.tid), 0, 0, 0, 0); errno != 0 {
				panic(fmt.Sprintf("ptrace sysemu failed: %v", errno))
			}
		} else {
			if _, _, errno := syscall.RawSyscall6(
				syscall.SYS_PTRACE,
				unix.PTRACE_SYSEMU,
				uintptr(t.tid), 0, 0, 0, 0); errno != 0 {
				panic(fmt.Sprintf("ptrace sysemu failed: %v", errno))
			}
		}

		// Wait for the syscall-enter stop.
		sig := t.wait(stopped)

		// Refresh all registers.
		if err := t.getRegs(regs); err != nil {
			panic(fmt.Sprintf("ptrace get regs failed: %v", err))
		}
		if err := t.getFPRegs(fpState, uint64(fpLen), useXsave); err != nil {
			panic(fmt.Sprintf("ptrace get fpregs failed: %v", err))
		}
		if err := t.getTLS(&tls); err != nil {
			panic(fmt.Sprintf("ptrace get tls failed: %v", err))
		}
		if !ac.SetTLS(uintptr(tls)) {
			panic(fmt.Sprintf("tls value %v is invalid", tls))
		}

		// Is it a system call?
		if sig == (syscallEvent | syscall.SIGTRAP) {
			// Ensure registers are sane.
			updateSyscallRegs(regs)
			return true
		} else if sig == syscall.SIGSTOP {
			// SIGSTOP was delivered to another thread in the same thread
			// group, which initiated another group stop. Just ignore it.
			continue
		}

		// Grab signal information.
		if err := t.getSignalInfo(&c.signalInfo); err != nil {
			// Should never happen.
			panic(fmt.Sprintf("ptrace get signal info failed: %v", err))
		}

		// We have a signal. We verify however, that the signal was
		// either delivered from the kernel or from this process. We
		// don't respect other signals.
		if c.signalInfo.Code > 0 {
			// The signal was generated by the kernel. We inspect
			// the signal information, and may patch it in order to
			// facilitate vsyscall emulation. See patchSignalInfo.
			patchSignalInfo(regs, &c.signalInfo)
			return false
		} else if c.signalInfo.Code <= 0 && c.signalInfo.Pid() == int32(os.Getpid()) {
			// The signal was generated by this process. That means
			// that it was an interrupt or something else that we
			// should bail for. Note that we ignore signals
			// generated by other processes.
			return false
		}
	}
}

func (s *subprocess) switchToAppLoop(sysemuRPCChan chan sysemuRPC) {
	// Lock the thread for ptrace operations.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Grab our new thread
	t := s.newThread()

	for rpc := range sysemuRPCChan {
		isSyscall := s.switchToAppLocked(t, rpc.c, rpc.ac)
		rpc.ret <- isSyscall
	}

	t.exit()
}

// switchToApp is called from the main SwitchToApp entrypoint.
//
// This function returns true on a system call, false on a signal.
func (s *subprocess) switchToApp(c *context, ac arch.Context) bool {
	ret := make(chan bool)
	defer close(ret)
	rpc := sysemuRPC{
		c:   c,
		ac:  ac,
		ret: ret,
	}
	s.mu.Lock()
	ch, ok := s.sysemuRPCPool[c.cid]
	if !ok {
		ch = make(chan sysemuRPC)
		s.sysemuRPCPool[c.cid] = ch
		go s.switchToAppLoop(ch)
		go func() { <-c.done; close(ch); delete(s.sysemuRPCPool, c.cid) }()
	}
	s.mu.Unlock()
	ch <- rpc
	return <-ret
}

type syscallRPC struct {
	sysno uintptr
	args  []arch.SyscallArgument
	ret   chan syscallRPCRet
}

type syscallRPCRet struct {
	val uintptr
	err error
}

func (s *subprocess) syscallLoop(ch chan syscallRPC) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	t := s.newThread()
	for rpc := range ch {
		val, err := t.syscallIgnoreInterrupt(&t.initRegs, rpc.sysno, rpc.args...)
		rpc.ret <- syscallRPCRet{val, err}
	}
	t.exit()
}

// syscall executes the given system call without handling interruptions.
func (s *subprocess) syscall(sysno uintptr, args ...arch.SyscallArgument) (uintptr, error) {
	ret := make(chan syscallRPCRet)
	defer close(ret)
	rpc := syscallRPC{
		sysno: sysno,
		args:  args,
		ret:   ret,
	}
	s.syscallRPCChan <- rpc
	r := <-ret
	return r.val, r.err
}

// MapFile implements platform.AddressSpace.MapFile.
func (s *subprocess) MapFile(addr usermem.Addr, f memmap.File, fr memmap.FileRange, at usermem.AccessType, precommit bool) error {
	var flags int
	if precommit {
		flags |= syscall.MAP_POPULATE
	}
	_, err := s.syscall(
		syscall.SYS_MMAP,
		arch.SyscallArgument{Value: uintptr(addr)},
		arch.SyscallArgument{Value: uintptr(fr.Length())},
		arch.SyscallArgument{Value: uintptr(at.Prot())},
		arch.SyscallArgument{Value: uintptr(flags | syscall.MAP_SHARED | syscall.MAP_FIXED)},
		arch.SyscallArgument{Value: uintptr(f.FD())},
		arch.SyscallArgument{Value: uintptr(fr.Start)})
	return err
}

// Unmap implements platform.AddressSpace.Unmap.
func (s *subprocess) Unmap(addr usermem.Addr, length uint64) {
	ar, ok := addr.ToRange(length)
	if !ok {
		panic(fmt.Sprintf("addr %#x + length %#x overflows", addr, length))
	}
	s.mu.Lock()
	for c := range s.contexts {
		c.mu.Lock()
		if c.lastFaultSP == s && ar.Contains(c.lastFaultAddr) {
			// Forget the last fault so that if c faults again, the fault isn't
			// incorrectly reported as a write fault. If this is being called
			// due to munmap() of the corresponding vma, handling of the second
			// fault will fail anyway.
			c.lastFaultSP = nil
			delete(s.contexts, c)
		}
		c.mu.Unlock()
	}
	s.mu.Unlock()
	_, err := s.syscall(
		syscall.SYS_MUNMAP,
		arch.SyscallArgument{Value: uintptr(addr)},
		arch.SyscallArgument{Value: uintptr(length)})
	if err != nil {
		// We never expect this to happen.
		panic(fmt.Sprintf("munmap(%x, %x)) failed: %v", addr, length, err))
	}
}

// PreFork implements platform.AddressSpace.PreFork.
func (s *subprocess) PreFork() {}

// PostFork implements platform.AddressSpace.PostFork.
func (s *subprocess) PostFork() {}
