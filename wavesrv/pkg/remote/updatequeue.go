package remote

import (
	"github.com/commandlinedev/apishell/pkg/base"
)

func startCmdWait(ck base.CommandKey) {
	GlobalStore.Lock.Lock()
	defer GlobalStore.Lock.Unlock()
	GlobalStore.CmdWaitMap[ck] = nil
}

func pushCmdWaitIfRequired(ck base.CommandKey, fn func()) bool {
	GlobalStore.Lock.Lock()
	defer GlobalStore.Lock.Unlock()
	fns, ok := GlobalStore.CmdWaitMap[ck]
	if !ok {
		return false
	}
	fns = append(fns, fn)
	GlobalStore.CmdWaitMap[ck] = fns
	return true
}

func runCmdUpdateFn(ck base.CommandKey, fn func()) {
	pushed := pushCmdWaitIfRequired(ck, fn)
	if pushed {
		return
	}
	fn()
}

func runCmdWaitFns(ck base.CommandKey) {
	for {
		fn := removeFirstCmdWaitFn(ck)
		if fn == nil {
			break
		}
		fn()
	}
}

func removeFirstCmdWaitFn(ck base.CommandKey) func() {
	GlobalStore.Lock.Lock()
	defer GlobalStore.Lock.Unlock()

	fns := GlobalStore.CmdWaitMap[ck]
	if len(fns) == 0 {
		delete(GlobalStore.CmdWaitMap, ck)
		return nil
	}
	fn := fns[0]
	GlobalStore.CmdWaitMap[ck] = fns[1:]
	return fn
}

func removeCmdWait(ck base.CommandKey) {
	GlobalStore.Lock.Lock()
	defer GlobalStore.Lock.Unlock()

	fns := GlobalStore.CmdWaitMap[ck]
	if len(fns) == 0 {
		delete(GlobalStore.CmdWaitMap, ck)
		return
	}
	go runCmdWaitFns(ck)
}