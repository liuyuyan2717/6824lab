
### 题目说明

题目地址：http://nil.csail.mit.edu/6.824/2021/labs/lab-shard.html

参考论文：http://nil.csail.mit.edu/6.824/2021/papers/raft-extended.pdf


构建一个sharding kv，将key分散到不同的分片当中，每一个分片负责一部分的key，每一个分片由一组服务器通过raft来提供备份同步。主要分为两部分，replica group和shard controller，replica group通过raft来负责一个shard的数据的备份和存储，shard controller用于决定replica group去对应哪一个shard：

* client询问shard controller去为一个key寻找一个replica group。
* replica group去询问shard controller自己应当对应哪个shard
* shard controller 也同样适用raft来进行容错
* shard controller 需要能够将shard重新分配给不同的 replica group，以进行负载均衡

**recpnfiguration**:

* 一个复制组内，所有组成员必须就相对于客户端 Put/Append/Get 请求发生重新配置的时间达成一致。例如，Put 可能与重新配置同时到达，防止一些成员认为自己还对该key负责，但是其他的成员认为该key已经由其他的shard和replica group负责，可以将重新配置同样作为一个日志存储在raft层，以达成共识
* 对于每个shard，同一时刻只能有一个replica group对其负责
* 进行reconfiguration后，需要将原本的key通过key进行迁移

## Lab4A

当分配情况发生改变时，shardctrler需要创建一份新的配置

RPC：

* **Join**：用于添加一个新的replica group,而一个replica group由多个raft节点组成,因此参数的为一个int(GID)到[]string（server列表）的map，接收到`Join`​后，shard controller需要创建一个包含新replica group的新配置，新的配置应该在所有组中尽可能平均地分配分片。并尽可能少地移动碎片来实现这一目标。如果有GID不是当前配置的一部分，那么shardctrler应该允许重用它(例如，允许GID加入，然后离开，然后再加入)。
* **Leave**：从配置中移除一个replica group，此时shard controller应当创建一个不包含该replica group的新的配置，将replica group的shard中的内容分配给其他的group，新配置需要尽量平均并且尽量少的移动
* **Move**:参数为一个shard number 和一个GID，即将该shard分配给GID对应的replica group
* **Query**：RPC参数是一个配置编号。shardctrler用具有该数字的配置来回复。如果编号是-1或者大于已知的最大配置编号，那么应该回复最新的一个配置。`Query(-1)`​的结果应该反应每一个在收到`Query(-1)`​之前shardctrler已经完成的`Join`​，`Leave`​或者`Move`​RPC。

第一个配置应当编号为0，不包含任何一个group，所有的 shard都应当分配GID 0，下一个配置则为通过Join创建，编号为1。 通常一个replica group会对应多个shard

**Hint**

* 从一个精简的kvraft服务器副本开始
* shard controller需要能够过滤来自客户端的重复的请求，就像 lab3当中一样
* 执行分片重新平衡的状态机中的代码需要具有确定性。在Go中，map的迭代顺序不确定。
* Go中的map是引用的。如果您将一个一个map类型的变量分配给了另一个变量，那么两个变量将引用同一个map。因此，如果希望基于以前的配置创建一个新的`Config`​，你需要创建一个新的map对象(使用`make()`​)以及分别复制键值

### 实现

lab中给出了Config的结构体：

```go
type Config struct {
	Num    int              // config number
	Shards [NShards]int     // shard -> gid
	Groups map[int][]string // gid -> servers[]
}
```

* Shards通过数组的形式来表明每个shard所对应的group，一个shard只能对应一个group，但是一个group可以负责多个shard
* Groups即为对应表明所有的group

**JoinHandler**​

用于将一个group加载到config当中，并令其负责一个shard：

1. 将其加载到Group当中，需要注意Go的map，需要创建一个新的Map来替代原本的Map
2. 创建一个Map，统计各个Shard的负载情况，如果为刚初始化的情况则直接返回
3. 对于当前的shard进行负载均衡，由于新加入的group一定为空闲的，因此去检测当前集群的状态，如果存在单个group负载过高的情况，则进行负载均衡
4. 返回新的配置，加入到server的[]config的最后

**leaveHandler**

从config当中移除一个group

1. 读取最新的config，将[]gids中的group一一删除
2. 负载均衡，将group负责的shard分配给其他的group

**moveHandler**​

1. 将传入的shard交给指定的gid的group

在整体实现上基本上和Lab3A大同小异，对着之前写的Lab3A抄就行了，server接收到了client的请求，之后生成一条日志，并且交给底层的raft去达成共识，同时有一个单独的协程在一直监听底层的raft的ApplyMsg，每当底层raft达成共识并commit之后，便获取到对应的日志中的op，进行处理，处理完成之后将结果通过一个协程去通知对应的server,server再将结果返回给client，完成一次处理的全过程

#### 负载均衡

对于负载均衡，最初想到大约有两种实现方式，分别为LRU或者平均分配

**LRU**

对于LRU，在Shard controller出维护一个链表，链表的头部为分配负责较少的shard 的group，尾部则为负载较高的group，当分配shard时就从头部获取一个group，令其负责当前的shard，但是这样存在的一个问题就是，无法去修改原本已经分配好的group，即如果最开始给一个group分配了十个shard，之后再新加入的group并不会为原本的group去分担shard来达到负载均衡的效果，因此LRU在此场景下并没法起到负载均衡的作用。

**平均分配**

另一种即为去尽可能的平均分配，当调用Join或者Leave导致group发生变动时，先根据统计的groupCount来计算出当前的平均负载状况，然后分别标记处高负载和低负载的group，然后将二者的shard进行一下迁移。

由于在负载均衡时，shard controller改变了统计的情况，同样的具体的group节点也需要完成存储的key的迁移操作，因此应当尽量少的去变动节点，只去协调单个高负载的和低负载的节点，其他节点尽量少的去变动

不过测试用例当中对于负载均衡有一定的要求，即最大值和最小值之间的差距不能够超过1，

**确定性**

Lab中有一个要求为对于每次的负载均衡，需要有确定性，即在每次负载均衡的结果是相同的，但是go中的map的迭代为一个随机性的，因此需要先对其进行排序，存放在一个切片当中，通过强制排序了保证确定性。

偷懒直接用一个冒泡排序，反正一共就十个shard,因此所对应group也不会太多，在性能上基本跑不出差距

```go
func sortGroupShard(GroupMap map[int]int) []int {
	length := len(GroupMap)

	gidSlice := make([]int, 0, length)

	// map转换成有序的slice
	for gid, _ := range GroupMap {
		gidSlice = append(gidSlice, gid)
	}

	// 让负载压力大的排前面
	// except: 4->3 / 5->2 / 6->1 / 7-> 1 (gids -> shard nums)
	for i := 0; i < length-1; i++ {
		for j := length - 1; j > i; j-- {

			if GroupMap[gidSlice[j]] < GroupMap[gidSlice[j-1]] || (GroupMap[gidSlice[j]] == GroupMap[gidSlice[j-1]] && gidSlice[j] < gidSlice[j-1]) {
				gidSlice[j], gidSlice[j-1] = gidSlice[j-1], gidSlice[j]
			}
		}
	}
	return gidSlice
}
```

在完成排序的基础上就可以对其进行负载均衡，对于想原本[3,3,1,1,1]的情况，经过负载均衡后的结果为:[2,2,2,2,1]，average = 1.8,通常average算出来的也不是整数，因此需要确定从哪一部分开始大于平均数，从哪一部分开始小于平均数。最简单的思路是先令所有的值全部为平均数向下取整，然后多余的数从最前开始一个group分配一个（共有lengh % Nshards）个，从而可以保证最终达到负载均衡，各个group的负载之间差距不超过1

## Lab4B

实现一个shardkv，与Lab3同样支持Get Put Append三种请求

通过key2shard()来找到一个合适的shard来放置当前的key

通过shard controller来为shard分配group，而当分配情况发生变化之后，在shard controller中进行重新分配，同时需要将相对应key从一个group迁移到另外一个group当中，防止客户端看到不一致的情况

与Lab3同样需要提供线性一致性

shard server 和shard controller只需要保证有大部分的节点存活且能够正常相互通信即可

一个shardkv server只能够作为一个replica group的成员，且永远不会改变

client向负责改key的replica group发送rpc，如果该group不负责该key，则告知client，client之后向shard controller去获取最新的配置，然后重试，

**Task1:**

只有一个分片分配，相较于lab3，最大的修改为让服务器检测什么时候发生配置，并开始接收匹配当前分片的key的请求。

在完成静态配置的情况之后，添加对动态配置的支持，需要令server能够监视配置的更改，一旦检测到，则进行shard迁移，将自身的key发送给对应的server。一旦发现自己不负责该shard，则立即停止对请求的响应，并且开始迁移数据，而当一个replica开始负责一个shard，他需要等待旧的shard data完成迁移才能开始接收该shard的请求。

**Task2:**

实现当配置发生变化时进行shard迁移，确保group中的所有的server全部进行，确保复制组中所有服务器在它们执行的操作操作序列的相同点执行迁移，以便它们都接受或拒绝并发客户端请求。

> * server通过轮训的方式来获取最新的配置，可以设置为100ms一次
> * 服务器之间需要相互发送RPC以便能够在配置更改期传递分片，分片控制器的`Config`​结构体包括了服务器的名字，但是你需要`labrpc.ClientEnd`​来发送RPC。你应该使用传递给`StartServer()`​的`make_end()`​函数来将一个服务器的名字转化为`CleintEnd`​。`shardkv/client.go`​包含这样的代码。

**Hints**​

* 在server.go当中实现从shard controller定期获取最新的配置，并且实现拒绝不是自己所负责的key的相关请求，
* 当请求的key不是自己所负责的，则返回一个ErrWrongGroup给client，需要考虑并发的重新配置的情况
* 按顺序依次只能进行一个重新配置
* 考虑shardkv client和server如何处理ErrWrongGroup
* 当一个server迁移至新的配置之后，依旧可以存储其不再拥有的key，一定程度上可以简化实现
* 考虑什么时候将日志条目从一个group发送至另外的一个group
* 对于shard的迁移，可以考虑发送整个Map，来简化实现
* 如果您的RPC处理程序之一在其应答中包含了作为服务器状态的一部分映射(比如kv键值对)，那么你有可能由于race而出bug。RPC系统必须读取到map以便将其发送给调用方，但它没有持续覆盖map的锁。你的服务器，会在RPC系统读取map的时候继续修改该map。解决方案是在RPC处理程序中回复该映射的副本。
* 如果你在Raft日志条目中放置了一个map或者slice，而你的键值对服务之后会看到该日志条目通过`applyCh`​，并将对map/slice的引用保存在键值对服务器的状态中。在你的kv服务器修改map/slice以及Raft在持久化时读取该map/slice时会产生了一个竞争。
* 在配置更改期间，一对副本组可能需要在它们之间互相移动分片。如果你看到死锁，这可能是一个来源。

### 整体架构

大致上可以分为三个部分：client、server、shard controller

* **client：**​直接对接用户，用户通过client端来发送请求，在功能上和Lab3的client几乎一致，实现Get PutAppend的RPC发送，但是由于当前的server端相较于Lab3实现了分片，因此需要先找到当前的key所属的shard，即向shard controller去发送 rpc，来获取当前的config，之后向该shard的replica group去发送RPC，在找到的了对应的shard之后的操作基本上和Lab3基本一致。Lab的初试代码当中为直接从已经存在的配置当中去获取配置，并且每次向server发送RPC之后都
* **shard controller：**​用于协调整个系统分片的情况，通过配置文件的形式来将replica group分给对应的shard，来完成key的存储，已经在Lab4A当中实现，

    * client每次向shard server发送RPC之后都向shard controller发送一次Query RPC来获取最新的配置
    * shard server也通过一个子协程来不断轮询，发送 Query RPC来尝试获取最新的配置
* **shard server:** 主体为Lab3B所实现的server，接受RPC之后去对Key进行处理

    * 需要和shard controller通信来拉取最新的配置
    * 进行了分片操作，因此如果不是自己的所负责的分片的可以应当拒绝处理，并告知client
    * 在发生配置更新时，应当进行shard迁移，通过RPC的方式，与其他的replica group进行通信，将自己负责的shard的相关信息发送给新的shard server（replica group）去处理，为了便捷直接将自己的整个map发送给新的shard server，并且此时可以继续持有原本的、已经不是由自己所负责的key，key相关的gc在challenge当中去实现
    * 共两个子协程，一个在监听raft层commit的相关信息，另一个去不断的轮询拉取最新的配置，对自身的配置进行更新。
    * 一个shard server对应一个底层raft节点，多个shard server组成一个replica group，去负责一个shard
    * 如向其他的shard server发送RPC还有拉取配置诸类操作是应当由当前的Leader去完成还是普通的follower也可以完成？

‍

#### Task1

由于只有一个shard的分配，因此shard server不需要去对配置有任何操作，只需要和Lab3B一样默默接受client的请求即可,把Lab3B能抄的全抄过去就能过第一个测试。

#### Task2

之后需要考虑添加shard的支持，在server端去添加一个子协程，不断的去拉取配置，如果发现为新配置，则应用于自身。

首先应当思考一个问题，就是从shard controller处获取到的config究竟有什么用？以及对谁有用。config用于确定replica group和shard的对应关系，因此只需要Leader来进行处理，Leader来判断当前整个replica Group所负责的shard，如果不是自己的shard中的key则直接拒绝掉，follower只需要默默的接受Leader发送来的Key的应用情况即可。因此在功能需求上follower是不需要config的，但是对于整个集群来说，需要保证线性一致性，即如果当前的配置发生改变，那么配置改变之前的Key还应当由当前的replica group负责，因此需要将配置改变的信息写入到raft的日志当中，通过日志来保证线性一致性，防止配置改变之前的请求被分配给了改变之后的shard。

因此先在`tryToPullConfig`​​当中将新配置的信息写入到raft日志当中，之后再在`applyMsgHandler`​​当中去处理，将新的配置应用于自身，值得注意的是，配置文件的处理应当和`Get`​​ `Put`​​ `Append`​​隔开，不去修改SeqId，防止影响client发送的命令的去重

### ShardKV

**存储方式**

先考虑最基本的问题即存储方式，一种较为简单的实现方式就是每一个Shard来分配一个对应的Map，并且对这个Shard绑定一个SeqIdMap，进行单独的去重,之后进行传输时以Shard为基本单位，将KVMap和SeqIdMap一同传输过去。

而所有的Key均为从客户端而来，因此只有客户端的相关操作才应当能够修改SeqIdMap，此处抽象为`ClientOp`​。

```go
type Shard struct {
	ShardStatus int
	KvDB map[string][string]
	SeqIdMap map[int64]int
}
statemachines [NShards]*Sha
```

### shard迁移

之后再去考虑shard迁移的问题：

* 在哪进行检测，发现需要进行迁移
* 由谁检测，发送方还是接收方？
* RPC都需要发送什么
* 如何阻断新的请求

对于shard迁移，主要有两种实现方式，即为Pull和Push，使用Pull的方法时，当一个group检测到自己要服务于新的shard，但是自己还未获得新的shard的Map时，此时向其他的group去尝试pull数据，而Push操作，即group检测是否有之前服务于自己但是在新配置当中不服务于自己的shard，如果有，则需要将其push给其他的节点。

**Pull**​

对于Pull方法，首先当group轮询检测到当前group处于pull状态时，发送RPC尝试拉取其他的shard。获取到shard之后，将其写入到raft当中，以达成共识，最终保证线性一致性的情况下改变状态机。如果要实现GC的话，之后在确认自己应用了相关的shard之后还需要再发送一次RPC，去告知对方的group将对应的shard的数据删除。因此一来一往对于一个shard而言就是两次RPC

**Push**​

push即为轮询到push之后发送一次RPC，要求对方去应用自己的shard，当RPC获取到响应之后，即可确认将自己原本的shard删除，在RPC的结果上处理较为麻烦，但是可以节省一次RPC的开销，总体来说在实现难度上二者基本一致。

**RPC调用端**

最终采用了Pull的方法，在调用端的大致的逻辑顺序如下：

1. 一个协程在不断的轮询，向shard controller去请求最新的配置，当获取到新的配置之后，就将其写入到raft当中，以保证线性一致性的更新配置，防止配置和请求之间违反线性一致的问题。
2. 当`applyMsgHandler`​​​从raft当中获取到了对应的配置更新的请求，此时根据新的配置来判断当前的group所处于的状态，将group的状态从`normal`​​​转换为`Pull`​​​或者`Push`​​​，之后再将新日志进行应用
3. 另外一个协程在不断的轮询检测当前的group的状态，一旦检测到group处于pull的状态，即找到需要pull的所有shard，发送RPC来pull数据

   > shards应当为一个map[int][]int的数据结构，map的key为gid，即从哪个group去获取shard，而value即为该group所负责的所有的shardId，例如，当前g1负责s1 s2,g2负责 s3 s4,g3负责 s5，此时g1 g2下线，在新配置中就将 s1-s4全部重新分配给g3，此时的数据结构即为:[g1->[s1,s2],g2->[s3,s4]]，因此需要并行的从g1 g2处拉取对应的shard集合。
>
4. 当RPC成功返回时，即可写入一条日志来请求将pull到的shard应用到自身的状态机之上，注意，为了保证各个shard之间的迁移过程互不干扰，因此需要保证分别写入日志，即对g1 g2拉取分两条写入到日志当中。这样在g1的shard应用到状态机之后如果g1之后重新分配了则g1即可重新对外提供服务，g1无需等待g2
5. 当从raft中获取到command之后，即可将shard应用到状态机当中，此时即可改变自身的状态为gcing，表明开始进入GC状态，而此时自身数据已经为最新的状态，已经可以对外提供读写服务，只是对方需要进行GC
6. 另外一个协程去单独检测GC相关的状态，如果检测到当前的状态为GCing，此时再去想对方group发送RPC，告知对方我已经完成了状态机的更新，此时你可以进行GC，删除掉之前的shard了，当得到正确的RPC响应之后，即可将自己的状态重新标记为normal，回归正常状态。

参数：发送当前的版本号和和需要获取的ShardIds

**RPC被调用端**

RPC被调用端负责将调用端需要的shard从自身获取出，并返回给调用端，

1. 先判断自身的config是否小于发送来的configNum，如果小于发送来的configNum，则证明自身的版本落后，可能存在脏数据，甚至当前需要拉去的分片都不是由自己所负责的，因此直接返回`ErrNotReady`​
2. 遍历发送来的ShardIds，深拷贝自身数据，放入到响应结构体当中，结构为一个Map，shardId->Shard，将kvDB和SeqIdMap一同拷贝

#### 节点状态

根据上述shard迁移的需求，节点大致需要以下四种状态：

* **Normal**​：正常且为默认状态，如果该shard处于自己的group的管理，则可以对外提供读写服务
* **Pull**: 即当前的group的正在管理此分片，但是此分片之前由其他的group负责，需要将数据从其他的group拉取过来之后才可对外提供正常服务
* **Push**：此分片在新配置当中已经不再受当前的group所管理，需要将其push给（被pull）其他的分片才可对外提供正常的读写服务
* **GCing**：当前的group已经获取到了最新的shard，完成了pull操作，可以对外提供读写服务，但是对方group（该shard的上一任group）还未将原本的shard数据删除，需要通过RPC告知对方将其删除之后才可将状态从GCing变回normal

#### 日志类型&Op

```go
type ClientOp struct {
	Key      string
	Value    string
	OpType   string
	ClientId int64
	SeqId    int
	ShardId  int
}
type ConfigOp struct {
	Config shardctrler.Config
}
type ShardOp struct {
	Type          int
	ConfigNum     int
	ShardIds      []int
	Datas         map[int]map[string]string
	RequestIdMaps map[int]map[int64]int
}
```

#### command

定义三种commandOp：

* ClientOp：处理客户端发来的相关操作，即`PUT`​ `GET`​ `APPEND`​并且对于客户端所发送的操作，需要进行去重处理，因此只在处理ClientOp的操作当中对`SeqIdMap`​进行更新，保证不影响对于配置和shard的处理
* ConfigOp：处理配置相关的消息，即拉取的新配置通过封装为一个ConfigOp，提交给raft
* ShardOp：处理shard迁移，以及迁移完成之后的gc操作。

#### 配置更新

* 需要保证当前并不存在正在迁移的shard才可以进行配置更新，即所有的节点全部为normal状态
* 配置的更新需要逐步进行，每次去获取下一个配置，之后写入到raft当中以获取共识，需要在拉取配置和更新配置的两个阶段均需要进行校验，即新配置比旧配置的configNum 大1才进行更新
* 额外保存lastConfig用于计算需要拉去的组

#### 分片清理

**RPC发送端**

分片清理相关的协程不断轮询所有shard的状态，当检测到当前状态为GCing时，则遍历所有状态为GCing的shard，调用RPC告知对方已经完成了shard的迁移，请求对方将对应的shard删除，并且将自身的状态从push转变为pull，开始对外提供正常服务

**RPC接受端**​

首先检测调用段的版本，如果自身版本高于调用者，则证明已经删除掉了，因此直接返回OK即可，否则写入一条日志来达到共识以进行删除

应用日志同样需要保证同配置版本，如果当前状态为GCing，则证明已经在正常对外提供服务，而如果为push，则将其恢复为默认状态，并且分配一个全新的kvDB和SeqIdMap

‍

#### 相关函数

#### 注意事项

* 为防止集群的分片状态被覆盖，因此当存在分片处于非normal的状态时（Push 或 Pull，应当停止将新的配置应用于自身
* 应当将不同group所属的分片数据独立起来，分别提交多条raft日志来维护状态，以保证对于多个shard的迁移能够相互独立，对应challange2
* 在apply配置时应当只修改对应的shard的状态，而不应该直接去进行shard迁移，gc等操作。对于shard迁移，shard清理等操作，需要单独开一个协程来去执行。
* ShardKV 不仅需要维护 currentConfig，还要保留 lastConfig，这样其他协程便能够通过 lastConfig，currentConfig 和所有分片的状态来不遗漏的执行任务
* 对于分片迁移，分片清除等操作，在发送RPC以及生成Op时应当携带一个版本，以便在raft重启应用日志时，新请求和旧日志之间不会相互覆盖

‍

### debug

**Snapshot**

目前通过了前两个测试，但是在进行TestSnapshot时有时会status设置出现问题导致，长时间维持pull 或者 push状态，之后一直无法正确处理请求。

此外读取snapshot之后的数据也不太正确，并且就算把测试中的重启的相关信息注释掉有时候也会获取到kong的key，也就是TestJoinAndLeave的测试并不够，单纯的进行join和leave也会出现bug

先检查一下并发问题

现在需要分析三个问题：

* 程序状态的问题，为何会拒绝给客户端响应
* 错误数据的问题
* Snapshot的问题

经过看日志发现，Group 101未跟上版本，但是依旧给了PullTask响应，最终导致自身并没有数据却能够响应PullTask，导致了对方的数据丢失，之后看了看代码结果是request 写成了response，，，，，，无语

经修改过之后前两个错误都不存在，即如果注释掉停机和恢复，能够通过snapshot的测试，现在可以确定问题出现在snapshot上

Snapshot条件写反了。。。。比较无语，至此可以稳定通过前三个测试。

**MissChange**​

第四个MissChange测试始终过不了，之后看了看日志发现是在leave之后再重新加入，但是并没有拉去到最新的配置，而导致自己的版本始终落后于他人，就无法进行响应。只能一直回复`ErrNotReady`​

现在存在两个问题：

* 某个group一直没能回到正确的SHARD_NORMAL的状态，导致无法拉去最行的版本，并且无法响应其他的Pull请求
* Apeend错误，会Append到其他的Key上来，最终导致结果错误

没能回到SHARD_NORMAL的状态为在GC处理时忘记设置了，设置了即可

Append错误为会将同于个value append到一个key上两次，重复的问题没能处理好

client端只发送了一遍，但是在server端被同一个group执行了两次

client发过去的SeqId为88，但是在server端却变成了10，

将原本的自增改为赋值即可解决

目前还会时不时的无法拉取新配置



对于unreliable的测试，一段时间过后就会检测不到Leader的状态，

