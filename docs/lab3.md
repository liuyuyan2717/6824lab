

## 题目说明

题目地址：http://nil.csail.mit.edu/6.824/2021/labs/lab-kvraft.html
参考论文：http://nil.csail.mit.edu/6.824/2021/papers/raft-extended.pdf

Lab3是根据Lab2所写的Raft来实现一个单个Raft集群的KV存储，难度上并不大，个人感觉和Lab1差不多，说一说整体架构和具体的实现

## 整体架构

首先整个KV存储采用的为client-server类型的架构，client接受用户的命令，之后以RPC的方式和server进行通信。在server端进行对数据的处理或者存储操作，client端负责与用户进行交流，接受用户所发出的请求。此处所描述的client端要区别于如redis客户端等独立的客户端。mini-raft的客户端应当作为整体存储的一部分，用户和client通过网络的方式进行通信，client再将对应的请求处理并发送至server端。

![](https://pic-bed-1309931445.cos.ap-nanjing.myqcloud.com/blog/image-20230318152429-53fb2z5.png)

在client端的实现比较简单，一个用户对应一个client即可，client端并不需要存在并发，只需要找到server端的Leader，之后只与Leader之间进行通信即可，如果发现当前正在通信的节点并非Leader要及时搜寻出新的Leader。

server端是整个KV存储的核心，其依赖于底层的Raft：

* 在server端存在多个节点，每个节点对应一个底层的Raft节点，与其进行通信，交换命令和日志提交的信息
* 只有Leader节点才能够对外提供服务，其他的节点只是作为Leader节点的冗余备份，用于故障恢复，在宕机后重新选举出新的Leader节点。因此在吞吐量上，想较于单机的形式吞吐量并不会有提升，甚至由于Raft内部的通信和等待日志提交，吞吐量和处理速度只会降低。
* server当中的Leader即为Raft的Leader，通过和底层的Raft询问来确认自己是否为Leader
* 通过Raft1的日志复制来完成各个节点之间的同步，follower通过底层同步而来的日志来使自身的状态机和Leader完成同步，并且可以通过快照的方式优化
* 通过WAL的形式，任何的请求都需要先写入到日志当中，包括读请求Get，只有通过Raft达成共识的才会被认可为有效的命令，最终被执行、应用于状态机、响应给client正确结果

整体架构图比较简单，简单画一下

![](https://pic-bed-1309931445.cos.ap-nanjing.myqcloud.com/blog/image-20230328151915-k8xy0jj.png)

对于单个server，只有Leader能够接受client的命令，然后调用start向下将命令封装成日志，然后等待底层的Raft对该条日志达成共识，之后再将结果应用于自身的状态机，然后返回给client。

Leader server接收到命令之后，将命令封装成日志之后交给client，然后通过一个单独的协程去监听底层的提交信息，当接收到提交的命令之后，将写请求应用于自身状态机，再通过一个channel将提交的命令告知给接收client命令的函数，之后将处理结果返回，Append()函数当中监听channel，并且设置一个定时器，如果长时间没有接收到对应的通知，那么就认为底层发生了宕机并重新选举Leader，因此告知client，自身并非Leader，让client再重新探查Leader

![](https://pic-bed-1309931445.cos.ap-nanjing.myqcloud.com/blog/image-20230328153711-bq9z2b0.png)

### 故障恢复

想象这样一个场景，server从client端接收到了一条Append命令，将其写入到日志之后就发生了宕机，然后Raft进行重新选举产生了另外一个Leader，之后将之前写入的日志应用到状态机，但是发送给前Leader的命令得不到回应导致RPC发送失败，之后client又尝试寻找新的Leader并再次发送命令，而这个命令又被新的Leader接受并执行、应用于状态机，因此同一条命令被执行了两次，因此需要对一个命令进行去重。

在Client端创建一个ClientId，并且发送命令时携带一个SeqId，来表示一条命令，在server端通过一个ClientId[SeqId]的map对client发送来的命令进行去重，保证同一条命令只会执行一遍。

而去重的位置个人认为在Append/Put函数或者在applyMsgHandler当中进行去重都正确而且也差不多，但是在命令函数当中进行去重的判断可以阻止重复命令写入到日志当中。但是就算写入了日志当中也可以通过快照来对垃圾日志进行清理，因此实际上都差不多，硬要说的话就是会增加创建快照的次数吧。

### 快照

在3B当中需要实现一个快照功能，以减少硬盘上的日志存储和用于宕机后的快速恢复，生成快照时需要将用于去重的Map一并和db map一同进行快照，保证重启之后通过快照仍能进行去重

快照的相关操作均为applyMsghandler进行处理：

* 同时当一条日志完成commit之后，上层`applyMsgHandler`​获取到了日志提交到信息，此时需要判断当前状态机的大小，是否需要创建快照，如果需要则创建快照。
* applyMsgHandler当检测到底层向上commit的快照那么就选择将快照解码，根据快照直接更新状态机
* 一般的命令日志和快照的生成在server层为线性的，但是在Raft层的处理和共识为并发的（分别通过start()和snapshot()创建），并且达成共识的速度也存在差异，因此可能在上层接收到提交信息时顺序已经完全错乱，因此需要保证一般日志和快照之间相互覆盖的情况

## 实现

### client

client在功能上比较简单，只需要对server的每一个节点发送命令进行探测，一旦找到了Leader就将其记录在自身，之后所有的命令就只发送给Leader，直到Leader宕机之后再重新搜索。在返回的结果上，OK/ErrNoKey都代表server端正确的处理了命令，即当前通信的为Leader，因此结束此次请求，而如果为ErrWrongLeader则证明对方不是Leader，需要重新进行探测

```go
type Clerk struct {
	servers []*labrpc.ClientEnd
	// You will have to modify this struct.
	clientId int64
	leaderId int
	seqId    int64
}
```

```go
func (ck *Clerk) Get(key string) string {
	Debug(dClient, "C calling the GetRPC key is %s", key)
	// You will have to modify this function.
	ck.seqId++
	getArgs := GetArgs{
		Key:      key,
		ClientId: ck.clientId,
		SeqId:    ck.seqId,
	}
	serverId := ck.leaderId
	for {
		getReply := GetReply{}
		ok := ck.servers[serverId].Call("KVServer.Get", &getArgs, &getReply)
		if !ok {
			Debug(dError, "C client send getRPC to server[%d] failed", serverId)
			serverId = (serverId + 1) % len(ck.servers)
		}
		if getReply.Err == ErrNoKey {
			ck.leaderId = serverId
			return ""
		} else if getReply.Err == ErrWrongLeader {
			serverId = (serverId + 1) % len(ck.servers)

		} else if getReply.Err == OK {
			ck.leaderId = serverId
			Debug(dCommit, "C Get OK key:%s,val:%s", key, getReply.Value)
			return getReply.Value
		}
	}
}
```

### server

server的结构体定义如下：

添加了msgChan用于接受提交的消息，memDb存储，seqIdMap去重，lastApplied防止相互覆盖

```go
type KVServer struct {
	mu          sync.Mutex
	me          int
	rf          *raft.Raft
	applyCh     chan raft.ApplyMsg
	dead        int32 // set by Kill()
	msgChan     map[int]chan Op
	memDb       map[string]string
	seqIdMap    map[int64]int64
	lastApplied int

	maxraftstate int // snapshot if log grows this big

	// Your definitions here.
}
```

命令函数所负责的就是将请求命令封装成一个日志，向下提交，监听对应日志的channel，等待结果回复给client

msgChan以日志的index作为key，监听自己创建的日志对应的channel

```go
func (kv *KVServer) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	// Your code here.
	if kv.killed() {
		Debug(dError, "get killed")
		reply.Err = ErrWrongLeader
		return
	}

	_, isLeader := kv.rf.GetState()
	if !isLeader {
		reply.Err = ErrWrongLeader
		return
	}
	op := Op{
		Key:      args.Key,
		Value:    args.Value,
		OpType:   args.Op,
		ClientId: args.ClientId,
		SeqId:    args.SeqId,
	}
	index, _, _ := kv.rf.Start(op)

	Debug(dLog, "S%d send a log PutAppend to raft type:%s key:%s value:%s", kv.me, args.Op, args.Key, args.Value)
	ch := kv.getMsgChan(index)
	defer func() {
		kv.mu.Lock()
		delete(kv.msgChan, index)
		kv.mu.Unlock()
	}()
	timer := time.NewTicker(400 * time.Millisecond)
	defer timer.Stop()
	select {
	case replyOp := <-ch:
		if op.ClientId != replyOp.ClientId || op.SeqId != replyOp.SeqId {
			Debug(dError, "S%d get the wrong Op", kv.me)
			reply.Err = ErrWrongLeader
		} else {
			reply.Err = OK
			Debug(dCommit, "S%d PutAppend OK key:%s value:%s", kv.me, args.Key, args.Value)
		}
	case <-timer.C:
		reply.Err = ErrWrongLeader
	}

}
```

#### applyMsgHandler

核心任务是接受底层Raft的commit信息，将其应用于状态机，同时还需要处理：

* 命令的去重
* 快照的创建
* 将命令或者快照应用于状态机
* 防止命令和快照之间相互覆盖

```go
func (kv *KVServer) applyMsgHandler() {
	if kv.killed() {
		return
	}
	for msg := range kv.applyCh {

		if msg.CommandValid {
			index := msg.CommandIndex
			kv.mu.Lock()
			op := msg.Command.(Op)
			if index <= kv.lastApplied {
				kv.mu.Unlock()
				continue
			}

			kv.lastApplied = index
			if !kv.isDuplicate(op.ClientId, op.SeqId) {

				kv.seqIdMap[op.ClientId] = op.SeqId
				if op.OpType == PUT {
					kv.memDb[op.Key] = op.Value
					Debug(dCommit, "S%d get PUT applyMsg from raft key:%s value:%s", kv.me, op.Key, op.Value)

				} else if op.OpType == APPEND {
					Debug(dCommit, "S%d get APPEND applyMsg from raft key:%s value:%s", kv.me, op.Key, op.Value)
					kv.memDb[op.Key] += op.Value
				}
			}
			if kv.maxraftstate != -1 && kv.rf.GetRaftState() > kv.maxraftstate {
				snapshot := kv.encodeSnapshot()
				Debug(dSnap, "S%d creates a snapshot lastApplied:%d snapshotIndex:%d", kv.me, kv.lastApplied, index)
				kv.rf.Snapshot(index, snapshot)
			}
			kv.mu.Unlock()
			kv.getMsgChan(msg.CommandIndex) <- op

			// nothing to do for GET Op
		}
		if msg.SnapshotValid {
			kv.mu.Lock()
			if msg.SnapshotIndex > kv.lastApplied {
				kv.decodeSnapshot(msg.Snapshot)
				kv.lastApplied = msg.SnapshotIndex
			}
			kv.mu.Unlock()

		}
	}

}
```







