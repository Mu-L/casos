package object

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/casosorg/casos/util"
	"xorm.io/xorm"
)

// MachineAuthTypeAgent marks a machine that joined a mesh hub on its own. The
// hub reaches it through the connection the machine keeps open to the hub,
// not through SSH, so it carries no credential here either.
const MachineAuthTypeAgent = "agent"

// MeshInvite lets machines join this hub's cloud. Only a hash of the secret
// is stored; the full invite is shown once, when it is created.
type MeshInvite struct {
	Owner       string `xorm:"varchar(100) notnull pk" json:"owner"`
	Name        string `xorm:"varchar(100) notnull pk" json:"name"`
	CreatedTime string `xorm:"varchar(100)" json:"createdTime"`
	ExpireTime  string `xorm:"varchar(100)" json:"expireTime"`
	UsesLeft    int    `xorm:"int" json:"usesLeft"`

	SecretHash string `xorm:"varchar(100)" json:"-"`
}

// MeshMember is the hub's record of a machine that joined, keyed like the
// machine it created. Its credential authenticates the machine's agent.
type MeshMember struct {
	Owner        string `xorm:"varchar(100) notnull pk" json:"owner"`
	Name         string `xorm:"varchar(100) notnull pk" json:"name"`
	CreatedTime  string `xorm:"varchar(100)" json:"createdTime"`
	Hostname     string `xorm:"varchar(255)" json:"hostname"`
	LastSeenTime string `xorm:"varchar(100)" json:"lastSeenTime"`

	CredentialHash string `xorm:"varchar(100)" json:"-"`
}

func hashMeshSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randomHex(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// AddMeshInvite creates an invite and returns its secret, which cannot be
// recovered afterwards.
func AddMeshInvite(owner string, ttl time.Duration, uses int) (*MeshInvite, string, error) {
	if uses <= 0 {
		return nil, "", fmt.Errorf("an invite must allow at least one machine to join")
	}
	name, err := randomHex(6)
	if err != nil {
		return nil, "", err
	}
	secret, err := randomHex(24)
	if err != nil {
		return nil, "", err
	}
	invite := &MeshInvite{
		Owner:       owner,
		Name:        name,
		CreatedTime: util.GetCurrentTime(),
		ExpireTime:  time.Now().Add(ttl).Format(time.RFC3339),
		UsesLeft:    uses,
		SecretHash:  hashMeshSecret(secret),
	}
	if _, err := ormer.Engine.Insert(invite); err != nil {
		return nil, "", err
	}
	return invite, secret, nil
}

func GetMeshInvites(owner string) ([]*MeshInvite, error) {
	invites := []*MeshInvite{}
	err := ormer.Engine.Desc("created_time").Find(&invites, &MeshInvite{Owner: owner})
	return invites, err
}

func DeleteMeshInvite(owner, name string) (bool, error) {
	affected, err := ormer.Engine.Delete(&MeshInvite{Owner: owner, Name: name})
	return affected != 0, err
}

// ConsumeMeshInvite spends one use of the invite named name, provided secret
// matches and it has not expired.
func ConsumeMeshInvite(name, secret string) (*MeshInvite, error) {
	session := ormer.Engine.NewSession()
	defer session.Close()
	if err := session.Begin(); err != nil {
		return nil, err
	}
	invite, err := consumeMeshInvite(session, name, secret)
	if err != nil {
		_ = session.Rollback()
		return nil, err
	}
	return invite, session.Commit()
}

func consumeMeshInvite(session *xorm.Session, name, secret string) (*MeshInvite, error) {
	invite := &MeshInvite{}
	existed, err := session.Where("name = ?", name).Get(invite)
	if err != nil {
		return nil, err
	}
	invalid := fmt.Errorf("the invite is not valid: ask the cloud's administrator for a new one")
	if !existed || subtle.ConstantTimeCompare([]byte(invite.SecretHash), []byte(hashMeshSecret(secret))) != 1 {
		return nil, invalid
	}
	if expire, err := time.Parse(time.RFC3339, invite.ExpireTime); err != nil || time.Now().After(expire) || invite.UsesLeft <= 0 {
		return nil, fmt.Errorf("the invite has expired or been used up: ask the cloud's administrator for a new one")
	}
	invite.UsesLeft--
	if _, err := session.Where("owner = ? AND name = ?", invite.Owner, invite.Name).Cols("uses_left").Update(invite); err != nil {
		return nil, err
	}
	return invite, nil
}

// AddMeshMember records a joined machine and returns the credential its agent
// authenticates with, which cannot be recovered afterwards.
func AddMeshMember(owner, name, hostname string) (string, error) {
	secret, err := randomHex(32)
	if err != nil {
		return "", err
	}
	member := &MeshMember{
		Owner:          owner,
		Name:           name,
		CreatedTime:    util.GetCurrentTime(),
		Hostname:       hostname,
		CredentialHash: hashMeshSecret(secret),
	}
	if _, err := ormer.Engine.Insert(member); err != nil {
		return "", err
	}
	return owner + "/" + name + "." + secret, nil
}

// VerifyMeshMemberCredential resolves an agent credential to its member, or
// nil when it matches none.
func VerifyMeshMemberCredential(credential string) (*MeshMember, error) {
	id, secret, ok := strings.Cut(credential, ".")
	if !ok {
		return nil, nil
	}
	owner, name, err := util.GetOwnerAndNameFromIdWithError(id)
	if err != nil {
		return nil, nil
	}
	member := &MeshMember{Owner: owner, Name: name}
	existed, err := ormer.Engine.Get(member)
	if err != nil || !existed {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(member.CredentialHash), []byte(hashMeshSecret(secret))) != 1 {
		return nil, nil
	}
	return member, nil
}

func TouchMeshMember(member *MeshMember) {
	member.LastSeenTime = util.GetCurrentTime()
	_, _ = ormer.Engine.Where("owner = ? AND name = ?", member.Owner, member.Name).Cols("last_seen_time").Update(member)
}

func GetMeshMembers(owner string) ([]*MeshMember, error) {
	members := []*MeshMember{}
	err := ormer.Engine.Desc("created_time").Find(&members, &MeshMember{Owner: owner})
	return members, err
}

func DeleteMeshMember(owner, name string) (bool, error) {
	affected, err := ormer.Engine.Delete(&MeshMember{Owner: owner, Name: name})
	return affected != 0, err
}
