# praft

> 一个简单的 Raft 共识性算法 Golang 实现

## Raft 实现

虽然 Raft 是 Paxos 简化而来，但它本身也足够复杂，罗马不是一天建成的，一定要耐住性子看论文、看动画、多实践。 

Raft 一共可分为三个大模块，分别是`选举`，`日志复制`和`持久化`。

这三个模块中，选举是核心点同时也是基本点；日志复制是 Leader 与 Follower 状态同步的手段；持久化是为了
增加容错，节点宕机后，恢复状态，并再次正确运行。

### 选举

当 Raft 节点（后面统一称为 Peer）运行启动时，初始状态为 Follower。Peer 有一个超时时间，
是心跳超时，当 Follower 在心跳定时器超时时未收到心跳消息，则 Follower 将
自动变更状态为 Candidate，并开始选举 Loop。

选举是一个循环，praft 的实现是 10ms 一轮，如果获得半数以上的选票则成为 Leader，如果没有则开始
下一轮的选举。

选举操作的核心是将 Follower 的状态变更为 Candidate，并给自己投上一票，然后通过 RPC 向其它 Peer
发送 RequestVote 请求，如果收到半数以上 Peer 的投票，就进阶成为 Leader。

一旦成为 Leader 就开始以心跳时间来向其它 Peer 发送心跳或者同步数据，其它 Peer 收到心跳后，检查任期，
然后成为 Follower。

### 日志复制

日志复制是 Leader 的独立操作。Leader 以固定的频率向其它 Follower 发送心跳包，即 AppendEntries，
当 AppendEntries 中没有日志的时候为心跳，其它情况下可看作日志同步。

日志复制是保证集群一致性的关键操作，Leader 的日志操作必须同等的应用在 Follower 上才能产生相同的结果。

Leader 通过 AppendEntries RPC 请求来完成日志复制，Leader 节点在内存中存储其它每个节点的 nextIndex 
和 matchIndex。nextIndex 来标识 AppendEntries 需要发送的日志条目，而 matchIndex 来更新当前 Leader 
的 commitIndex。

Follower 收到 AppendEntries 后，对比节点中的日志条目与请求中的日志条目，并向内存中添加日志，随后根据
Leader 的 commitIndex 和添加的日志数据来更新自己的 commitIndex。

### 持久化

持久化是 Raft 中较为简单的一部分，核心内容是持久化如下三个变量：

```go
// persistent Raft state
currentTerm int        // 当前任期
votedFor    int        // 给谁投过票
log         []LogEntry // 日志
```

编码并持久化的方式多种多样，可选择最为简单的 json。持久化的时机是需要把握的，主要有如下几点：

- 节点启动时，检查是否有持久化数据，若存在，则恢复
- 在节点提交日志时，将数据持久化到磁盘

## 其它

本文实现的 Raft 参考 Eli，但这个 Raft 实现似乎在持久化上有问题，后续待更新，若需要更加严谨的实现
可参考 [mit6.824](http://nil.csail.mit.edu/6.824/2020/labs/lab-raft.html)

## 参考资料
- https://eli.thegreenplace.net/2020/implementing-raft-part-1-elections/
- http://thesecretlivesofdata.com/raft/