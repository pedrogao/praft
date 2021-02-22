package raft

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const DebugCM = 1

type LogEntry struct {
	Command interface{}
	Term    int
}

type CommitEntry struct {
	Command interface{}
	Index   int
	Term    int
}

type CMState int

const (
	Follower CMState = iota
	Candidate
	Leader
	Dead
)

func (s CMState) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	case Dead:
		return "Dead"
	default:
		panic("unreachable")
	}
}

type ConsensusModule struct {
	mu      sync.Mutex // 锁
	id      int        // 当前模块id
	peerIds []int      // 集群端点id
	server  *Server    // RPC server

	commitChan chan<- CommitEntry // 提交队列

	newCommitReadyChan chan struct{} // 新提交准备

	// persistent Raft state
	currentTerm int        // 当前任期
	votedFor    int        // 给谁投过票
	log         []LogEntry // 日志

	// volatile state
	commitIndex        int       // 已提交日志序号
	lastApplied        int       // 最后应用日志序号
	state              CMState   // 当前角色状态
	electionResetEvent time.Time // 选举时间

	// volatile Raft leader state
	nextIndex  map[int]int // 下一个日志序号
	matchIndex map[int]int // 已匹配日志序号
}

func NewConsensusModule(id int, peerIds []int, server *Server, ready <-chan interface{}, commitChan chan<- CommitEntry) *ConsensusModule {
	cm := new(ConsensusModule)
	cm.id = id
	cm.peerIds = peerIds
	cm.server = server
	cm.commitChan = commitChan
	cm.newCommitReadyChan = make(chan struct{}, 16) // 带一个 16 的缓冲，防止过度等待
	cm.state = Follower                             // 刚开始是 Follower，超时后变成 Candidate
	cm.votedFor = -1
	cm.commitIndex = -1
	cm.lastApplied = -1
	cm.nextIndex = make(map[int]int)
	cm.matchIndex = make(map[int]int)

	go func() {
		<-ready
		cm.mu.Lock()
		cm.electionResetEvent = time.Now() // 重置选举时间
		cm.mu.Unlock()
		cm.runElectionTimer() // 开始选举
	}()

	go cm.commitChanSender()

	return cm
}

// leader 提交 command 日志
func (cm *ConsensusModule) Submit(command interface{}) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cm.dlog("Submit received by %v: %v", cm.state, command)
	if cm.state == Leader {
		cm.log = append(cm.log, LogEntry{
			Command: command,
			Term:    cm.currentTerm,
		})
		cm.dlog("... log=%v", cm.log)
		return true
	}
	return false
}

func (cm *ConsensusModule) Report() (id int, term int, isLeader bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.id, cm.currentTerm, cm.state == Leader
}

func (cm *ConsensusModule) Stop() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.state = Dead // 死亡
	cm.dlog("becomes Dead")
	close(cm.newCommitReadyChan)
}

// 开始选举
func (cm *ConsensusModule) runElectionTimer() {
	timeoutDuration := cm.electionTimeout()
	cm.mu.Lock()
	termStarted := cm.currentTerm
	cm.mu.Unlock()
	cm.dlog("election timer started (%v), term=%d", timeoutDuration, termStarted)
	// 10ms 下一轮
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		<-ticker.C

		cm.mu.Lock()
		// 当前状态既不是 Candidate 也不是 Follower，即 Follower 或者 Dead，则无需选举，直接退出
		if cm.state != Candidate && cm.state != Follower {
			cm.dlog("in election timer state=%s, bailing out", cm.state)
			cm.mu.Unlock()
			return
		}
		// 如果任期发生了更新，结束当前选举，进入下一轮选举
		if termStarted != cm.currentTerm {
			cm.dlog("in election timer term changed from %d to %d, bailing out", termStarted, cm.currentTerm)
			cm.mu.Unlock()
			return
		}
		// 选举超时，则触发下一次选举
		if elapsed := time.Since(cm.electionResetEvent); elapsed >= timeoutDuration {
			cm.startElection() // 开始选举
			cm.mu.Unlock()
			return
		}
		cm.mu.Unlock()
	}
}

// 开始选举
func (cm *ConsensusModule) startElection() {
	cm.state = Candidate // 变更状态
	cm.currentTerm += 1
	savedCurrentTerm := cm.currentTerm
	cm.electionResetEvent = time.Now() // 选举时间重置
	cm.votedFor = cm.id                // 给自己投票
	cm.dlog("becomes Candidate (currentTerm=%d); log=%v", savedCurrentTerm, cm.log)

	var votesReceived int32 = 1 // 已收到票数，自己的一票

	// 发送选票请求 RPC
	for _, peerId := range cm.peerIds {
		go func(peerId int) {
			cm.mu.Lock()
			savedLastLogIndex, savedLastLogTerm := cm.lastLogIndexAndTerm()
			cm.mu.Unlock()
			args := RequestVoteArgs{
				Term:         savedCurrentTerm,
				CandidateId:  cm.id,
				LastLogIndex: savedLastLogIndex,
				LastLogTerm:  savedLastLogTerm,
			}
			cm.dlog("sending RequestVote to %d: %+v", peerId, args)
			var reply RequestVoteReply
			if err := cm.server.Call(peerId, "ConsensusModule.RequestVote", args, &reply); err == nil {
				cm.mu.Lock()
				defer cm.mu.Unlock()
				cm.dlog("received RequestVoteReply %+v", reply)
				// 发送了投票请求，但是我的状态已经发生了改变，不再是 Candidate，那么直接退出
				if cm.state != Candidate {
					cm.dlog("while waiting for reply, state=%v", cm.state)
					return
				}
				// 如果回复者的任期比发送者的任期大，那么我将成为追随者
				if reply.Term > savedCurrentTerm {
					cm.dlog("term out of date in RequestVoteReply")
					cm.becomeFollower(reply.Term)
					return
				} else if reply.Term == savedCurrentTerm { // 如果回复者的任期与请求者的任期相同
					if reply.VotedGranted { // 且请求者收到了投票
						votes := int(atomic.AddInt32(&votesReceived, 1))
						if votes*2 > len(cm.peerIds)+1 { // 如果获得了半数以上的投票
							cm.dlog("wins election with %d votes", votes)
							cm.startLeader() // 成为 leader
							return
						}
					}
				}
			}
		}(peerId)
	}
	// 开始另一次选举
	go cm.runElectionTimer()
}

// 随机返回选举超时时间，150ms ～ 300ms
func (cm *ConsensusModule) electionTimeout() time.Duration {
	if len(os.Getenv("RAFT_FORCE_MORE_REELECTION")) > 0 && rand.Intn(3) == 0 {
		return time.Duration(150) * time.Millisecond
	} else {
		return time.Duration(150+rand.Intn(150)) * time.Millisecond
	}
}

// Debug 输出日志信息
func (cm *ConsensusModule) dlog(format string, args ...interface{}) {
	if DebugCM > 0 {
		format = fmt.Sprintf("[%d] ", cm.id) + format
		log.Printf(format, args...)
	}
}

// 选举投票请求
type RequestVoteArgs struct {
	Term         int
	CandidateId  int
	LastLogIndex int
	LastLogTerm  int
}

// 选举投票回复
type RequestVoteReply struct {
	Term         int
	VotedGranted bool
}

// 请求投票 RPC
func (cm *ConsensusModule) RequestVote(args RequestVoteArgs, reply *RequestVoteReply) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	// 死亡状态，直接返回
	if cm.state == Dead {
		return nil
	}
	lastLogIndex, lastLogTerm := cm.lastLogIndexAndTerm()
	cm.dlog("RequestVote: %+v [currentTerm=%d, votedFor=%d, log index/term=(%d, %d)]", args, cm.currentTerm, cm.votedFor, lastLogIndex, lastLogTerm)
	// 如果对方的任期大于当前任期，直接变成 Follower
	if args.Term > cm.currentTerm {
		cm.dlog("... term out of date in RequestVote")
		cm.becomeFollower(args.Term)
	}
	// 如果对方的任期等于当前任期 且 （当前未投票 或者 投票的人正是发请求的人）
	// 那么将当前任期的一票投给请求者
	if cm.currentTerm == args.Term &&
		(cm.votedFor == -1 || cm.votedFor == args.CandidateId) &&
		(args.LastLogTerm > lastLogTerm || (args.LastLogTerm == lastLogTerm && args.LastLogIndex >= lastLogIndex)) {
		reply.VotedGranted = true
		cm.votedFor = args.CandidateId
		cm.electionResetEvent = time.Now() // 票已投，当前选举结束，进入下一个选举
	} else { // 其它的情况，都不进行投票
		reply.VotedGranted = false
	}
	reply.Term = cm.currentTerm
	cm.dlog("... RequestVote: %+v", reply)
	return nil
}

// 当前节点成为 Follower
func (cm *ConsensusModule) becomeFollower(term int) {
	cm.dlog("becomes Follower with term=%d; log=%v", term, cm.log)
	cm.state = Follower                // 状态
	cm.currentTerm = term              // 请求者的任期
	cm.votedFor = -1                   // 成为追随者，我票谁也没投
	cm.electionResetEvent = time.Now() // 重置选举时间

	go cm.runElectionTimer() // 重新开始选举
}

// 成为 Leader
func (cm *ConsensusModule) startLeader() {
	cm.state = Leader
	cm.dlog("becomes Leader; term=%d, log=%v", cm.currentTerm, cm.log)

	go func() {
		// 50ms 一次
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		// 向 follower 发送心跳
		for {
			// 发送心跳
			cm.leaderSendHeartbeats()
			<-ticker.C

			cm.mu.Lock()
			if cm.state != Leader {
				cm.mu.Unlock()
				return
			}
			cm.mu.Unlock()
		}
	}()
}

type AppendEntriesArgs struct {
	Term     int
	LeaderId int

	PrevLogIndex int
	PrevLogTerm  int
	Entries      []LogEntry
	LeaderCommit int
}

type AppendEntriesReply struct {
	Term    int
	Success bool
}

// 处理追加日志请求
func (cm *ConsensusModule) AppendEntries(args AppendEntriesArgs, reply *AppendEntriesReply) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.state == Dead {
		return nil
	}
	cm.dlog("AppendEntries: %+v", args)
	// 如果请求者的任期比我大，直接成为 Follower
	if args.Term > cm.currentTerm {
		cm.dlog("... term out of date in AppendEntries")
		cm.becomeFollower(args.Term)
	}
	reply.Success = false
	if args.Term == cm.currentTerm { // 任期相同
		//Q: What if this peer is a leader - why does it become a follower to another leader?
		//
		//A: Raft guarantees that only a single leader exists in any given term.
		// If you carefully follow the logic of RequestVote and the code in startElection that sends RVs,
		// you'll see that two leaders can't exist in the cluster with the same term.
		// This condition is important for candidates that find out that
		// another peer won the election for this term.
		if cm.state != Follower { // 收到了心跳请求，但是我不是 Follower，那么直接成为 Follower
			cm.becomeFollower(args.Term)
		}
		// 收到了 leader 心跳，则重置选举时间
		cm.electionResetEvent = time.Now()

		if args.PrevLogIndex == -1 || // -1 代表未同步过日志
			// 同步的日志序号小于当前端点的日志长度 且 同步的任期与日志的任期是一致的
			(args.PrevLogIndex < len(cm.log) && args.PrevLogTerm == cm.log[args.PrevLogIndex].Term) {
			reply.Success = true                    // 心跳成功
			logInsertIndex := args.PrevLogIndex + 1 // 插入日志的序号
			newEntriesIndex := 0                    // Entries 序号，与 logInsertIndex 一一对应

			for {
				if logInsertIndex >= len(cm.log) || newEntriesIndex >= len(args.Entries) {
					break
				}
				if cm.log[logInsertIndex].Term != args.Entries[newEntriesIndex].Term {
					break
				}
				logInsertIndex++
				newEntriesIndex++
			}
			// 待插入的日志个数得小于心跳中的日志数量
			if newEntriesIndex < len(args.Entries) {
				cm.dlog("... inserting entries %v from index %d", args.Entries[newEntriesIndex:], logInsertIndex)
				cm.log = append(cm.log[:logInsertIndex], args.Entries[newEntriesIndex:]...)
				cm.dlog("... log is now: %v", cm.log)
			}
			// 如果 leader 的提交序号大于当前节点的提交序号
			if args.LeaderCommit > cm.commitIndex {
				cm.commitIndex = intMin(args.LeaderCommit, len(cm.log)-1) // 更新 commitIndex
				cm.dlog("... setting commitIndex=%d", cm.commitIndex)
				cm.newCommitReadyChan <- struct{}{}
			}
		}
	}

	reply.Term = cm.currentTerm
	cm.dlog("AppendEntries reply: %+v", *reply)
	return nil
}

// leader 发送心跳
func (cm *ConsensusModule) leaderSendHeartbeats() {
	cm.mu.Lock()
	savedCurrentTerm := cm.currentTerm
	cm.mu.Unlock()

	for _, peerId := range cm.peerIds {
		go func(peerId int) {
			cm.mu.Lock()
			ni := cm.nextIndex[peerId] // peer 的下一个日志序列
			preLogIndex := ni - 1      // 上一个日志序列
			preLogTerm := -1           // 上一个日志任期
			if preLogIndex >= 0 {
				preLogTerm = cm.log[preLogIndex].Term
			}
			entries := cm.log[ni:] // 序号后面的都是需要同步的日志

			args := AppendEntriesArgs{
				Term:         savedCurrentTerm,
				LeaderId:     cm.id,
				PrevLogIndex: preLogIndex,
				PrevLogTerm:  preLogTerm,
				Entries:      entries,
				LeaderCommit: cm.commitIndex,
			}
			cm.mu.Unlock()
			cm.dlog("sending AppendEntries to %v: ni=%d, args=%+v", peerId, ni, args)

			var reply AppendEntriesReply
			if err := cm.server.Call(peerId, "ConsensusModule.AppendEntries", args, &reply); err == nil {
				cm.mu.Lock()
				defer cm.mu.Unlock()
				if reply.Term > savedCurrentTerm { // 如果接收者的任期大于 leader 的任期
					cm.dlog("term out of date in heartbeat reply")
					cm.becomeFollower(reply.Term) // 那么 leader 转变成为 follower
					return
				}
				// 发送心跳成功
				if cm.state == Leader && savedCurrentTerm == reply.Term {
					if reply.Success { // 心跳发送成功
						cm.nextIndex[peerId] = ni + len(entries)         // 更新 nextIndex
						cm.matchIndex[peerId] = cm.nextIndex[peerId] - 1 // 更新 matchIndex
						cm.dlog("AppendEntries reply from %d success: nextIndex := %v, matchIndex := %v", peerId, cm.nextIndex, cm.matchIndex)

						savedCommitIndex := cm.commitIndex
						// 从 commitIndex + 1 开始，依次查看，更新 commitIndex
						for i := cm.commitIndex + 1; i < len(cm.log); i++ {
							if cm.log[i].Term == cm.currentTerm { // 一定得是当前任期的日志
								matchCount := 1
								for _, peerId := range cm.peerIds {
									if cm.matchIndex[peerId] >= i { // matchIndex >= i 即是日志已经应用
										matchCount++
									}
								}
								if matchCount*2 > len(cm.peerIds)+1 { // 如果超过半数的 peer 已经应用了日志
									cm.commitIndex = i // 则更新 commitIndex
								}
							}
						}
						// 更新了 commitIndex
						if cm.commitIndex != savedCommitIndex {
							cm.dlog("leader sets commitIndex := %d", cm.commitIndex)
							cm.newCommitReadyChan <- struct{}{}
						}
					} else {
						cm.nextIndex[peerId] = ni - 1 // 如果日志同步失败，则向后一步，然后继续下一次同步
						cm.dlog("AppendEntries reply from %d failed: nextIndex := %d", peerId, ni-1)
					}
				}
			}
		}(peerId)
	}
}

// 当 commitIndex 更新
func (cm *ConsensusModule) commitChanSender() {
	for range cm.newCommitReadyChan {
		cm.mu.Lock()
		savedTerm := cm.currentTerm
		savedLastApplied := cm.lastApplied
		var entries []LogEntry
		if cm.commitIndex > cm.lastApplied {
			entries = cm.log[cm.lastApplied+1 : cm.commitIndex+1] // 需要应用的日志
			cm.lastApplied = cm.commitIndex
		}
		cm.mu.Unlock()
		cm.dlog("commitChanSender entries=%v, savedLastApplied=%d", entries, savedLastApplied)

		for i, entry := range entries {
			cm.commitChan <- CommitEntry{
				Command: entry.Command,
				Index:   savedLastApplied + i + 1,
				Term:    savedTerm,
			}
		}
	}
	cm.dlog("commitChanSender done")
}

// 获得最后的日志序号和任期
func (cm *ConsensusModule) lastLogIndexAndTerm() (int, int) {
	if len(cm.log) > 0 {
		lastIndex := len(cm.log) - 1
		return lastIndex, cm.log[lastIndex].Term
	} else {
		return -1, -1 // -1 表示还没有任何数据
	}
}

func intMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}
