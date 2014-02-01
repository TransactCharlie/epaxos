package replica

// This file implements instance module.
// @assumption:
// - When a replica pass in the message to instance methods, we assume that the
//    internal fields of message is readable only and safe to reference to.
// @assumption(02/01/14):
// - When a new instance is created, it's at nilstatus.
// @decision (01/31/14):
// - Status has precedence. An accepted instance won't handle pre-accept even if
// - the pre-accept carries larger ballot.
// @decision (02/01/14):
// - Executed won't be included in instance statuses anymore.
// - Executed will be recorded in a flag. This will simplify the state machine.

import (
	"fmt"

	"github.com/go-epaxos/epaxos/data"
)

var _ = fmt.Printf

// ****************************
// *****  CONST ENUM **********
// ****************************

// instance status
const (
	nilStatus uint8 = iota + 1
	preparing
	preAccepted
	accepted
	committed
)

// ****************************
// ***** TYPE STRUCT **********
// ****************************

type Instance struct {
	cmds   data.Commands
	seq    uint32
	deps   data.Dependencies
	status uint8
	ballot *data.Ballot

	info         *InstanceInfo
	recoveryInfo *RecoveryInfo

	// local information
	replica  *Replica
	id       uint64
	executed bool
}

// bookkeeping struct for recording counts of different messages and some flags
type InstanceInfo struct {
	isFastPath     bool
	preAcceptCount int
	acceptCount    int
}

type RecoveryInfo struct {
	preAcceptedCount  int
	replyCount        int
	maxAcceptedBallot *data.Ballot

	cmds         data.Commands
	deps         data.Dependencies
	status       uint8
	formerStatus uint8
}

// ****************************
// **** NEW INSTANCE **********
// ****************************

func NewInstance(replica *Replica, instanceId uint64) (i *Instance) {
	i = &Instance{
		replica:      replica,
		id:           instanceId,
		deps:         replica.makeInitialDeps(),
		info:         NewInstanceInfo(),
		recoveryInfo: NewRecoveryInfo(),
		ballot:       data.NewBallot(0, 0, 0),
		status:       nilStatus,
	}
	return i
}

func NewInstanceInfo() *InstanceInfo {
	return &InstanceInfo{
		isFastPath: true,
	}
}

func NewRecoveryInfo() *RecoveryInfo {
	return &RecoveryInfo{}
}

// *********************************
// ******** INSTANCE FIELDS  *******
// *********************************
func (i *Instance) isAtStatus(status uint8) bool {
	return i.status == status
}

func (i *Instance) isAfterStatus(status uint8) bool {
	return i.status > status
}

func (i *Instance) isAtOrAfterStatus(status uint8) bool {
	return i.status >= status
}

func (i *Instance) freshlyCreated() bool {
	return i.ballot.GetEpoch() == 0
}

func (i *Instance) ableToFastPath() bool {
	return i.info.isFastPath && i.ballot.IsInitialBallot()
}

func (i *InstanceInfo) reset() {
	i.isFastPath = true
	i.preAcceptCount = 0
	i.acceptCount = 0
}

func (i *Instance) initRecoveryInfo() {
	i.recoveryInfo.replyCount = 0
	i.recoveryInfo.maxAcceptedBallot = i.ballot.GetCopy()
	i.recoveryInfo.cmds = i.cmds
	i.recoveryInfo.deps = i.deps
	i.recoveryInfo.status = i.status
	i.recoveryInfo.formerStatus = i.status
	// preacceptcount is used to count N/2 identical initial preaccepts.
	if i.isAtStatus(preAccepted) {
		i.recoveryInfo.preAcceptedCount = 0
	} else {
		i.recoveryInfo.preAcceptedCount = 1
	}
}

// ******************************
// ****** State Processing ******
// ******************************

// NilStatus exists for:
// - the instance is newly created when
// - - received a proposal first time and only once. (sender)
// - - received pre-accept, accept, commit, prepare the first time. (receiver)
// - - required by commit dependencies and transitioning to preparing. (sender)
// - the instance is not newly created when
// - - after reverted back from `preparing`(sender -> receiver)
// - - received prepare and waiting for further message. (receiver)
func (i *Instance) nilStatusProcess(m Message) (action uint8, msg Message) {
	defer i.checkStatus(preAccepted, accepted, committed, preparing)

	if !i.isAtStatus(nilStatus) {
		panic("")
	}

	switch content := m.Content().(type) {
	case *data.Propose:
		return i.handlePropose(content)
	case *data.PreAccept:
		if content.Ballot.Compare(i.ballot) < 0 {
			return i.rejectPreAccept()
		}
		return i.handlePreAccept(content)
	case *data.Accept:
		if content.Ballot.Compare(i.ballot) < 0 {
			return i.rejectAccept()
		}
		return i.handleAccept(content)
	case *data.Commit:
		return i.handleCommit(content)
	case *data.Prepare:
		if content.Ballot.Compare(i.ballot) < 0 {
			return i.rejectPrepare()
		}
		return i.handlePrepare(content)
	case *data.PrepareReply:
		if i.freshlyCreated() {
			panic("Never send prepare before but receive prepare reply")
		}
		return action, nil
	case *data.PreAcceptReply, *data.AcceptReply, *data.PreAcceptOk:
		panic("")
	default:
		panic("")
	}
}

// preAccepted instance can be of two stands:
// - as a sender
// - as a receiver
func (i *Instance) preAcceptedProcess(m Message) (action uint8, msg Message) {
	defer i.checkStatus(preAccepted, accepted, committed)

	if !i.isAtStatus(preAccepted) {
		panic("")
	}

	switch content := m.Content().(type) {
	case *data.PreAccept:
		if content.Ballot.Compare(i.ballot) < 0 {
			return i.rejectPreAccept()
		}
		return i.handlePreAccept(content)
	case *data.Accept:
		if content.Ballot.Compare(i.ballot) < 0 {
			return i.rejectAccept()
		}
		return i.handleAccept(content)
	case *data.Commit:
		return i.handleCommit(content)
	case *data.Prepare:
		if content.Ballot.Compare(i.ballot) < 0 {
			return i.rejectPrepare()
		}
		return i.handlePrepare(content)

	case *data.PreAcceptReply:
		if content.Ballot.Compare(i.ballot) < 0 {
			// ignore stale PreAcceptReply
			return noAction, nil
		}
		return i.handlePreAcceptReply(content)
	case *data.PreAcceptOk:
		if !i.ballot.IsInitialBallot() {
			return noAction, nil // ignore stale reply
		}
		return i.handlePreAcceptOk(content)
	case *data.AcceptReply:
		panic("")
	case *data.PrepareReply:
		if i.ballot.IsInitialBallot() {
			panic("")
		}
		return noAction, nil
	default:
		panic("")
	}
}

// accepted instance can be of two roles:
// - as a sender
// - - It will handle corresponding accept reply. On majority votes, it will
// - - transition to committed and broadcast commit.
// - as a receiver
// - - It will handle accept, prepare with larger ballot, and commit.
func (i *Instance) acceptedProcess(m Message) (action uint8, msg Message) {
	defer i.checkStatus(accepted, committed)

	if !i.isAtStatus(accepted) {
		panic("")
	}

	switch content := m.Content().(type) {
	case *data.PreAccept:
		return i.rejectPreAccept()
	case *data.Accept:
		if content.Ballot.Compare(i.ballot) < 0 {
			return i.rejectAccept()
		}
		return i.handleAccept(content)
	case *data.Commit:
		return i.handleCommit(content)
	case *data.Prepare:
		if content.Ballot.Compare(i.ballot) < 0 {
			return i.rejectPrepare()
		}
		return i.handlePrepare(content)

	case *data.AcceptReply:
		if content.Ballot.Compare(i.ballot) < 0 {
			return noAction, nil // ignore stale PreAcceptReply
		}
		return i.handleAcceptReply(content)
	case *data.PreAcceptReply, *data.PreAcceptOk:
		return noAction, nil // ignore stale replies
	case *data.PrepareReply:
		if i.ballot.IsInitialBallot() {
			panic("")
		}
		return noAction, nil // ignore stale replies
	default:
		panic("")
	}
}

// committed instance will
// - reject all request messages (pre-accept, accept),
// - handle prepare to help it find committed,
// - ignore others
func (i *Instance) committedProcess(m Message) (action uint8, msg Message) {
	defer i.checkStatus(committed)

	if !i.isAtStatus(committed) {
		panic("")
	}

	switch content := m.Content().(type) {
	case *data.PreAccept:
		return i.rejectPreAccept()
	case *data.Accept:
		return i.rejectAccept()
	case *data.Prepare:
		return i.handlePrepare(content)
	case *data.PreAcceptReply, *data.PreAcceptOk, *data.AcceptReply, *data.PrepareReply, *data.Commit:
		return noAction, nil // ignore stale replies
	default:
		panic("")
	}
}

// preparing instance could only acts as a sender.
// It handles most kinds of messages (in some conditions with larger ballot) and
// ignores all replies except prepare reply.
func (i *Instance) preparingProcess(m Message) (action uint8, msg Message) {
	defer i.checkStatus(preparing, preAccepted, accepted, committed)

	if !i.isAtStatus(preparing) || i.recoveryInfo == nil {
		panic("")
	}

	switch content := m.Content().(type) {
	case *data.PreAccept:
		if content.Ballot.Compare(i.ballot) < 0 {
			return i.rejectPreAccept()
		}
		return i.handlePreAccept(content)

	case *data.Accept:
		if content.Ballot.Compare(i.ballot) < 0 {
			return i.rejectAccept()
		}
		return i.handleAccept(content)
	case *data.Commit:
		return i.handleCommit(content)
	case *data.Prepare:
		// the instance itself is the first one to have ballot of this
		// magnitude. It can't receive others having the same
		if content.Ballot.Compare(i.ballot) == 0 {
			panic("")
		}
		if content.Ballot.Compare(i.ballot) < 0 {
			return i.rejectPrepare()
		}
		return i.revertPrepare(content)

	case *data.PrepareReply:
		if content.Ballot.Compare(i.ballot) < 0 {
			return noAction, nil
		}
		return i.handlePrepareReply(content)
	case *data.PreAcceptReply:
		if i.recoveryInfo.formerStatus < preAccepted {
			panic("")
		}
		return noAction, nil
	case *data.PreAcceptOk:
		if i.recoveryInfo.formerStatus < preAccepted {
			panic("")
		}
		return noAction, nil
	case *data.AcceptReply:
		if i.recoveryInfo.formerStatus < accepted {
			panic("")
		}
		// ignore delayed replies
		return noAction, nil
	default:
		panic("")
	}
}

// ******************************
// ****** Reject Messages *******
// ******************************

// -------- REJECT CONDITIONS --------
// When someone rejected a message (pre-accept, accept, prepare), it implied that
// the rejector had larger ballot than the message sent. In such a case, we just need
// to return {ok: false, rejector's ballot, and relevant ids}
// -----------------------------------

// PreAccept reply:
// - ok : false
// - Ballot: self (ballot)
// - Ids
func (i *Instance) rejectPreAccept() (action uint8, reply *data.PreAcceptReply) {
	return replyAction, i.makePreAcceptReply(false, 0, nil)
}

// rejectAccept rejects the Accept request with a AcceptReply:
// - Ok: false
// - Ballot: self.ballot
// - ReplicaId: self.replica.id
// - InstanceId: self.id
// - other fields: undefined
func (i *Instance) rejectAccept() (action uint8, reply *data.AcceptReply) {
	return replyAction, &data.AcceptReply{
		Ok:         false,
		ReplicaId:  i.replica.Id,
		InstanceId: i.id,
		Ballot:     i.ballot.GetCopy(),
	}
}

// Prepare reply:
// - ok : false
// - Ballot: self (ballot)
// - relevant Ids
func (i *Instance) rejectPrepare() (action uint8, reply *data.PrepareReply) {
	return replyAction, &data.PrepareReply{
		Ok:         false,
		ReplicaId:  i.replica.Id,
		InstanceId: i.id,
		Ballot:     i.ballot.GetCopy(),
	}
}

// ******************************
// ****** Handle Message  *******
// ******************************

// a propose will broadcasted to fast quorum in pre-accept message.
func (i *Instance) handlePropose(p *data.Propose) (action uint8, msg *data.PreAccept) {
	if p.Cmds == nil || !i.freshlyCreated() {
		panic("")
	}
	seq, deps := i.replica.findDependencies(p.Cmds)

	i.cmds = p.Cmds
	i.seq = seq
	i.deps = deps
	i.ballot = i.replica.makeInitialBallot()

	i.enterPreAcceptedAsSender()

	return fastQuorumAction, &data.PreAccept{
		ReplicaId:  i.replica.Id,
		InstanceId: i.id,
		Cmds:       p.Cmds.GetCopy(),
		Seq:        seq,
		Deps:       deps.GetCopy(),
		Ballot:     i.ballot.GetCopy(),
	}
}

// When handling pre-accept, instance will set its ballot to newer one, and
// update seq, deps if find any change.
// Reply: pre-accept-OK if no change in deps; otherwise a normal pre-accept-reply.
// The pre-accept-OK contains just one field, which is a serilization optimization.
func (i *Instance) handlePreAccept(p *data.PreAccept) (action uint8, msg Message) {
	if p.Ballot.Compare(i.ballot) < 0 {
		panic("")
	}

	i.enterPreAcceptedAsReceiver()
	i.cmds, i.ballot = p.Cmds, p.Ballot
	seq, deps, changed := i.replica.updateDependencies(p.Cmds, p.Seq, p.Deps, p.ReplicaId)

	if changed {
		i.seq, i.deps = seq, deps
		return replyAction, i.makePreAcceptReply(true, seq, deps)
	}
	// not initial leader
	if p.Ballot.GetNumber() != 0 {
		return replyAction, i.makePreAcceptReply(true, seq, deps)
	}
	// pre-accept-ok for possible fast quorum commit
	return replyAction, &data.PreAcceptOk{
		InstanceId: i.id,
	}
}

// handlePreAcceptOk handles PreAcceptOks,
// one replica will receive PreAcceptOks only if it's the initial leader,
// on receiving this message,
// it will increase the preAcceptCount, and do broadcasts if:
// - the preAcceptCount >= the size of fast quorum, and all replies are
// the same, then it will broadcast Commits,
// - the preAcceptCount >= N/2(not including the sender), and not all replies are equal,
// then it will broadcast Accepts,
// - otherwise, do nothing
func (i *Instance) handlePreAcceptOk(p *data.PreAcceptOk) (action uint8, msg Message) {
	panic("")
}

// handlePreAcceptReply:
// receiving negative pre-accept reply (ok == false), for someone has larger ballot
// - update ballot
// - instance becomes a receiver from a sender
// receiving corresponding pre-accept reply (ok == true)
// - union seq, deps, and update counts
// Broadcast cases:
// - receiving == fast quorum replies with same deps and seq,
//    and the instance itself is initial leader,
//    then broadcast Commit (fast path)
// - receiving >= N/2 replies (not including the sender itself),
//    and not satisfying above condition,
//    then broadcast Accepts (slow path, Paxos Accept Phase)
// - Otherwise do nothing.
func (i *Instance) handlePreAcceptReply(p *data.PreAcceptReply) (action uint8, msg Message) {
	if p.Ballot.Compare(i.ballot) < 0 {
		panic("")
	}
	if p.Ballot.Compare(i.ballot) > 0 {

		// [*] there may be stale but large ballots,
		// if we receive such ballots, that means there may be another newer proposer,
		// so we'd better step down by increasing our own ballot so we can ignore
		// the following replies.
		if p.Ok {
			panic("")
		}
		i.ballot = p.Ballot
		return noAction, nil
	}

	// update relevants
	i.ballot = p.Ballot
	i.info.preAcceptCount++
	if same := i.deps.Union(p.Deps); !same {
		// We take difference of deps only for replies from other replica.
		if i.info.preAcceptCount > 1 {
			i.info.isFastPath = false
		}
	}

	if i.info.preAcceptCount >= i.replica.fastQuorum() && i.ableToFastPath() {
		// TODO: persistent
		i.enterCommitted()

		return broadcastAction, &data.Commit{
			Cmds:       i.cmds.GetCopy(),
			Seq:        i.seq,
			Deps:       i.deps.GetCopy(),
			ReplicaId:  i.replica.Id,
			InstanceId: i.id,
		}
	} else if i.info.preAcceptCount >= int(i.replica.Size/2) && !i.ableToFastPath() {
		// TODO: persistent
		i.enterAcceptedAsSender()

		return broadcastAction, &data.Accept{
			Cmds:       i.cmds.GetCopy(),
			Seq:        i.seq,
			Deps:       i.deps.GetCopy(),
			ReplicaId:  i.replica.Id,
			InstanceId: i.id,
			Ballot:     i.ballot.GetCopy(),
		}
	}
	return noAction, nil
}

// handleAccept handles Accept messages (receiver)
// Update:
// - cmds, seq, ballot
// action: reply an AcceptReply message, with:
// - Ok = true
// - everything else = instance's fields
func (i *Instance) handleAccept(a *data.Accept) (action uint8, msg *data.AcceptReply) {
	if a.Ballot.Compare(i.ballot) < 0 {
		panic("")
	}
	if i.isAfterStatus(accepted) ||
		i.isAtStatus(accepted) && a.Ballot.Compare(i.ballot) == 0 {
		panic("")
	}

	i.cmds, i.seq, i.deps, i.ballot = a.Cmds, a.Seq, a.Deps, a.Ballot
	i.enterAcceptedAsReceiver()

	return replyAction, &data.AcceptReply{
		Ok:         true,
		ReplicaId:  i.replica.Id,
		InstanceId: i.id,
		Ballot:     i.ballot.GetCopy(),
	}
}

// handleAcceptReply handles AcceptReplies as sender,
// Update:
// - ballot
// Broadcast event happens when:
// if receiving majority replies with ok == true,
//    then broadcast Commit
// otherwise: do nothing.
func (i *Instance) handleAcceptReply(a *data.AcceptReply) (action uint8, msg *data.Commit) {
	if a.Ballot.Compare(i.ballot) < 0 {
		panic("")
	}

	// negative reply
	if i.ballot.Compare(a.Ballot) < 0 {

		// [*] there may be stale but large ballots,
		// if we receive such ballots, that means there may be another newer proposer,
		// so we'd better step down by increasing our own ballot so we can ignore
		// the following replies.
		if a.Ok {
			panic("")
		}
		i.ballot = a.Ballot
		return noAction, nil
	}

	if !a.Ok {
		panic("")
	}

	i.info.acceptCount++
	if i.info.acceptCount >= int(i.replica.Size/2) {
		i.enterCommitted()
		return broadcastAction, &data.Commit{
			Cmds:       i.cmds.GetCopy(),
			Seq:        i.seq,
			Deps:       i.deps.GetCopy(),
			ReplicaId:  i.replica.Id,
			InstanceId: i.id,
		}
	}
	return noAction, nil
}

// TODO: need testing
func (i *Instance) handleCommit(c *data.Commit) (action uint8, msg Message) {
	if i.isAtOrAfterStatus(committed) {
		panic("")
	}

	i.cmds, i.seq, i.deps = c.Cmds, c.Seq, c.Deps
	i.enterCommitted()

	// TODO: Do we need to clear unnecessary objects to save more memory?
	// TODO: persistent
	return noAction, nil
}

func (i *Instance) revertPrepare(p *data.Prepare) (action uint8, msg *data.PrepareReply) {
	i.status = i.recoveryInfo.formerStatus
	return i.handlePrepare(p)
}

func (i *Instance) handlePrepare(p *data.Prepare) (action uint8, msg *data.PrepareReply) {
	oldBallot := i.ballot.GetCopy()

	// We optimize the case of committed instance in reply with ok=true message
	// and do not update self ballot (useless for committed ones)
	if !i.isAtStatus(committed) {
		if p.Ballot.Compare(i.ballot) <= 0 { // cannot be equal or smaller
			panic(fmt.Sprintln("prepare ballot: ", p.Ballot, i.ballot))
		}
		i.ballot = p.Ballot
	}

	cmds := data.Commands(nil)
	// if the preparing instance know the commands (i.e. it has been told
	// beforehand), we won't bother to serialize it over the network.
	if p.NeedCmdsInReply {
		cmds = i.cmds.GetCopy()
	}

	return replyAction, &data.PrepareReply{
		Ok:             true,
		ReplicaId:      i.replica.Id,
		InstanceId:     i.id,
		Status:         i.status,
		Seq:            i.seq,
		Cmds:           cmds,
		Deps:           i.deps.GetCopy(),
		Ballot:         p.Ballot.GetCopy(),
		OriginalBallot: oldBallot,
	}
}

func (i *Instance) handlePrepareReply(p *data.PrepareReply) (action uint8, msg Message) {
	// if the reply has larger ballot, then we can step down
	panic("")
}

// checkStatus checks the status of the instance
// it panics if the instance's status is not as expected
func (i *Instance) checkStatus(statusList ...uint8) {
	ok := false
	for _, status := range statusList {
		if i.isAtStatus(status) {
			ok = true
			break
		}
	}
	if !ok {
		panic("")
	}
}

// ****************************
// ******* Make Message *******
// ****************************

func (i *Instance) makePreAcceptReply(ok bool, seq uint32, deps data.Dependencies) *data.PreAcceptReply {
	return &data.PreAcceptReply{
		Ok:         ok,
		ReplicaId:  i.replica.Id,
		InstanceId: i.id,
		Seq:        seq,
		Deps:       deps,
		Ballot:     i.ballot.GetCopy(),
	}
}

// *******************************
// ******* State Transition ******
// *******************************

func (i *Instance) enterNilStatus() {
	i.checkStatus(preparing)
	i.status = nilStatus
}

func (i *Instance) enterPreAcceptedAsSender() {
	i.checkStatus(nilStatus, preparing)
	i.status = preAccepted
	i.info.reset()
}

func (i *Instance) enterPreAcceptedAsReceiver() {
	i.checkStatus(nilStatus, preparing, preAccepted)
	i.status = preAccepted
}

func (i *Instance) enterAcceptedAsSender() {
	i.checkStatus(nilStatus, preAccepted, preparing)
	i.status = accepted
	i.info.reset()
}

func (i *Instance) enterAcceptedAsReceiver() {
	i.checkStatus(nilStatus, preAccepted, preparing, accepted)
	i.status = accepted
}
func (i *Instance) enterCommitted() {
	i.checkStatus(nilStatus, preAccepted, preparing, accepted)
	i.status = committed
}
func (i *Instance) enterPreparing() {
	i.checkStatus(nilStatus, preAccepted, preparing, accepted)
	i.initRecoveryInfo()
	i.status = preparing
}
