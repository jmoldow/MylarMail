package main

import "net"
import "fmt"
import "net/rpc"
import "log"
import "sync"
import "os"
import "syscall"
import "math/rand"
import "time"
import "hash/fnv"
import "math/big"
import "crypto/rand"

const (
  OK = "OK"
  ErrWrongCoordinator = "ErrWrongCoordinator"
  Debug = 0
)

func DPrintf(format string, a ...interface{}) (n int, err error) {
  if Debug > 0 {
    log.Printf(format, a...)
  }
  return
}

/*
****************************************************
Data Types
****************************************************
*/

type MMDatabase struct {
  mu sync.Mutex
  l net.Listener
  me int
  dead bool // for testing
  unreliable bool // for testing
  servers []string
  nServers int
  nReplicas int // Number of replicas wanted
  handoffMessages []*Message // Messages that need to be handed off
}

type Message struct {
  id MessageID
  // Whether or not this message needs to be handed off to another node later
  isHandoff bool
  handoffDestination string
  handoffUsername string
  data string
  collection string
}

/*
****************************************************
API from Mylar/Meteor
****************************************************
*/

// Returns an ordered slice of servers in order they should be considered as coordinator
func (db *MMDatabase) GetCoordinatorList(username string) []string {
  initialIndex := db.getCoordinatorIndex(username)
  output := make([]string, 0)
  
  for i := initialIndex; i < len(db.servers); i++ {
    output = append(output, db.servers[i])
  }
  
  for i := 0; i < initialIndex; i++ {
    output = append(output, db.servers[i])
  }
  
  return output
}

// Returns success once nReplicas replicas are stored in the system
func (db *MMDatabase) CoordinatorPut(username string, id RequestID, message Message) Err {
  // Assert that this should be coordinator
  if db.getCoordinatorIndex(username) != db.me && !message.isHandoff {
    return ErrWrongCoordinator
  }
  
  totalReplicas := 0
  replicaLocations := make(map[int]bool)
  handoffTargets := make(map[int]bool)
  
  // Send to all N replicas except for this one (the coordinator)
  for totalReplicas < db.nReplicas-1 {
    for i, server := range(db.GetCoordinatorList(username)) {
      if !replicaLocations[i] && i != db.me {
        // Set Hinted Handoff
        handoffTarget := db.getHandoffTarget(username, i, replicaLocations, handoffTargets)
        if handoffTarget == -1 {
          message.isHandoff = false
        } else {
          message.isHandoff = true
          message.handoffDestination = db.servers[i]
          message.handoffUsername = username
          handoffTargets[handoffTarget] = true
        }
        // Set up args and reply
        args := new(ReplicaPutArgs)
        reply := new(ReplicaPutReply)
        args.Username = username
        args.Msg = message
        args.Handoff = false
        
        ok := call(server, "MMDatabase.ReplicaPut", args, reply)
      
        if ok && reply.Err == OK {
          totalReplicas++
          replicaLocations[i] = true
        }
      }
      
      if totalReplicas >=  db.nReplicas {
        break
      }
      
    }
  }
  
  // There should now be (at least) nReplicas-1 replicas in the system.
  // Replicate at the N-th server (this one / the coordinator),
  // then return success.
  db.LocalPut(username, message)
  totalReplicas++
  replicaLocations[db.me] = true
  
  return OK
}

/*
****************************************************
API to Mylar/Meteor
****************************************************
*/

func (db *MMDatabase) LocalPut(username string, msg Message) Err {
  // TODO
  return OK
}

func (db *MMDatabase) LocalDelete(username string, id MessageID) Err {
  // TODO
  return OK
}

/*
****************************************************
API to Servers
****************************************************
*/

func (db *MMDatabase) ReplicaPut(args *ReplicaPutArgs, reply *ReplicaPutReply) error {
  message := args.Msg
  // if message is satisfying a handoff, mark it as not needing handoff
  if args.Handoff {
    message.isHandoff = false
  }
  
  // Do Local Put
  db.LocalPut(args.Username, message)
  
  // if message needs to be handed off, store in list of messages that need handing off
  if message.isHandoff {
    db.handoffMessages = append(db.handoffMessages, &message)
  }
  reply.Err = OK
  return nil
}

/*
****************************************************
API Helpers
****************************************************
*/

// Returns a copy of slice without message at index
func removeMessage(slice []*Message, index int) []*Message {
  maxIndex := len(slice)-1
  
  lastElem := slice[maxIndex]
  slice[maxIndex] = slice[index]
  slice[index] = lastElem
  
  return slice[:maxIndex]
}

func (db *MMDatabase) runHandoffLoop() {
  for !db.dead {
    for i, message := range db.handoffMessages {
      // Set up args and reply
      args := new(ReplicaPutArgs)
      reply := new(ReplicaPutReply)
      args.Username = message.handoffUsername
      args.Msg = *message
      args.Handoff = true
        
      ok := call(message.handoffDestination, "MMDatabase.ReplicaPut", args, reply)
      
      if ok && reply.Err == OK {
        // Handoff successful, delete message
        db.handoffMessages = removeMessage(db.handoffMessages, i)
        break
      } else {
        time.Sleep(1000*time.Millisecond)
      }
    }
  }
}

// Returns index of first server that should be chosen as coordinator
func (db *MMDatabase) getCoordinatorIndex(username string) int {
  return int(hash(username) % uint32(db.nServers))
}

// Returns what the current handoff target should be with respect to replicaLocations
// Returns -1 if no handoff
// Assumes currentIndex is in range [0,nReplicas-1]
func (db *MMDatabase) getHandoffTarget(username string, currentIndex int, replicaLocations map[int]bool, handoffTargets map[int]bool) int {
  wrap := false
  firstReplica := db.getCoordinatorIndex(username)
  lastReplica := firstReplica + db.nReplicas
  if lastReplica >= db.nServers {
    wrap = true
    lastReplica = lastReplica % db.nServers
  }
  
  // Return -1 if in proper range
  if wrap {
    if currentIndex >= firstReplica || currentIndex <= lastReplica {
      return -1
    }
  } else {
    if firstReplica <= currentIndex && currentIndex <= lastReplica {
      return -1
    }
  }
  
  // Otherwise, target first one on priority list with no replica or targeted handoff yet
  i := firstReplica
  for {
    if !replicaLocations[i] && !handoffTargets[i] {
      return i
    }
    i++
    if i >= db.nServers {
      i = i % db.nServers
    }
  }
}

/*
****************************************************
API Dispatch Methods
****************************************************
*/

// Serves RPC calls from other database instances
func serveRPC() {
  for db.dead == false {
    conn, err := db.l.Accept()
    if err == nil && db.dead == false {
      if db.unreliable && (rand.Int63() % 1000) < 100 {
        // discard the request.
        conn.Close()
      } else if db.unreliable && (rand.Int63() % 1000) < 200 {
        // process the request but force discard of reply.
        c1 := conn.(*net.UnixConn)
        f, _ := c1.File()
        err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
        if err != nil {
          fmt.Printf("shutdown: %v\n", err)
        }
        go rpcs.ServeConn(conn)
      } else {
        go rpcs.ServeConn(conn)
      }
    } else if err == nil {
      conn.Close()
    }
    if err != nil && db.dead == false {
      fmt.Printf("MMDatabase(%v) accept: %v\n", me, err.Error())
      db.kill()
    }
  }
}

/*
****************************************************
Helper Functions
****************************************************
*/

func sameID(id1 RequestID, id2 RequestID) bool {
  return id1.ClientID == id2.ClientID && id1.Seq == id2.Seq
}

func hash(s string) uint32 {
  h := fnv.New32a()
  h.Write([]byte(s))
  return h.Sum32()
}

func nrand() int64 {
  max := big.NewInt(int64(1) << 62)
  bigx, _ := rand.Int(rand.Reader, max)
  x := bigx.Int64()
  return x
}

/*
****************************************************
Helper Data Types
****************************************************
*/

type Err string

type ReplicaPutArgs struct {
  Username string
  Msg Message
  // Whether this ReplicaPut call is satisfying a Handoff (as opposed to being in top nReplicas of priority list)
  Handoff bool
}

type ReplicaPutReply struct {
  Err Err
}

type RequestID struct {
  ClientID int64
  Seq int64
}

/*
****************************************************
Start and Kill Code
****************************************************
*/

// tell the server to shut itself down.
func (db *MMDatabase) kill() {
  DPrintf("Kill(%d): die\n", db.me)
  db.dead = true
  db.l.Close()
}

//
// servers[] contains the ports of the set of
// servers that will cooperate via Paxos to
// form the fault-tolerant key/value service.
// me is the index of the current server in servers[].
// 
func StartServer(servers []string, me int) *MMDatabase {
  // call gob.Register on structures you want
  // Go's RPC library to marshall/unmarshall.

  db := new(MMDatabase)
  db.dead = false
  db.me = me
  db.servers = servers
  db.nServers = len(servers)
  db.nReplicas = 3
  db.handoffMessages = make([]*Message, 0)
  db.id = nrand()

  go db.runHandoffLoop()

  rpcs := rpc.NewServer()
  rpcs.Register(db)

  os.Remove(servers[me])
  l, e := net.Listen("unix", servers[me]);
  if e != nil {
    log.Fatal("listen error: ", e);
  }
  db.l = l


  // please do not change any of the following code,
  // or do anything to subvert it.

  go serveRPC()

  return db
}

func main() {
  fmt.Printf("Test Main\n")
}