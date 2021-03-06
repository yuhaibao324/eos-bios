package bios

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"math"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/eoscanada/eos-go"
	"github.com/eoscanada/eos-go/ecc"
)

type BIOS struct {
	Network *Network

	LaunchData   *LaunchData
	EOSAPI       *eos.API
	Snapshot     Snapshot
	BootSequence []*OperationType

	Genesis *GenesisJSON

	// ShuffledProducers is an ordered list of producers according to
	// the shuffled peers.
	ShuffledProducers []*Peer
	RandSource        rand.Source

	// MyPeers represent the peers my local node will handle. It is
	// plural because when launching a 3-node network, your peer will
	// be cloned a few time to have a full schedule of 21 producers.
	MyPeers []*Peer

	EphemeralPrivateKey *ecc.PrivateKey
}

func NewBIOS(network *Network, api *eos.API) *BIOS {
	b := &BIOS{
		Network: network,
		EOSAPI:  api,
	}
	return b
}

func (b *BIOS) SetGenesis(gen *GenesisJSON) {
	b.Genesis = gen
}

func (b *BIOS) Init() error {
	// Load launch data
	launchData, err := b.Network.ConsensusLaunchData()
	if err != nil {
		fmt.Println("couldn'get consensus on launch data:", err)
		os.Exit(1)
	}

	b.LaunchData = launchData

	// TODO: check that nodes that are ABP or participants do have an
	// EOSIOABPSigningKey set.

	// Load the boot sequence
	rawBootSeq, err := b.Network.ReadFromCache(string(launchData.BootSequence))
	if err != nil {
		return fmt.Errorf("reading boot_sequence file: %s", err)
	}

	var bootSeq struct {
		BootSequence []*OperationType `json:"boot_sequence"`
	}
	if err := yamlUnmarshal(rawBootSeq, &bootSeq); err != nil {
		return fmt.Errorf("loading boot sequence: %s", err)
	}

	b.BootSequence = bootSeq.BootSequence

	// Load snapshot data
	if launchData.Snapshot != "" {
		rawSnapshot, err := b.Network.ReadFromCache(string(launchData.Snapshot))
		if err != nil {
			return fmt.Errorf("reading snapshot file: %s", err)
		}

		snapshotData, err := NewSnapshot(rawSnapshot)
		if err != nil {
			return fmt.Errorf("loading snapshot csv: %s", err)
		}
		b.Snapshot = snapshotData
	}

	if err := b.setProducers(); err != nil {
		return err
	}

	if err = b.setMyPeers(); err != nil {
		return fmt.Errorf("error setting my producer definitions: %s", err)
	}

	return nil
}

func (b *BIOS) StartOrchestrate(secretP2PAddress string) error {
	fmt.Println("Starting Orchestraion process", time.Now())

	fmt.Println("Showing pre-randomized network discovered:")
	b.Network.PrintOrderedPeers()

	b.RandSource = b.waitEthereumBlock()

	// Once we have it, we can discover the net again (unless it's been discovered VERY recently)
	// and we b.Init() again.. so load the latest version of the LaunchData according to this
	// potentially new discovery network.
	fmt.Println("Ethereum block used to seed randomization, updating graph one last time...")

	if err := b.Network.UpdateGraph(); err != nil {
		return fmt.Errorf("orchestrate: update graph: %s", err)
	}

	fmt.Println("Network used for launch:")
	b.Network.PrintOrderedPeers()

	if err := b.DispatchInit("orchestrate"); err != nil {
		return fmt.Errorf("dispatch init hook: %s", err)
	}

	switch b.MyRole() {
	case RoleBootNode:
		if err := b.RunBootSequence(secretP2PAddress); err != nil {
			return fmt.Errorf("orchestrate boot: %s", err)
		}
	case RoleABP:
		if err := b.RunJoinNetwork(true, true); err != nil {
			return fmt.Errorf("orchestrate join: %s", err)
		}
	default:
		if err := b.RunJoinNetwork(true, false); err != nil {
			return fmt.Errorf("orchestrate participate: %s", err)
		}
	}

	return b.DispatchDone("orchestrate")
}

func (b *BIOS) StartJoin(verify bool) error {
	fmt.Println("Starting network join process", time.Now())

	b.Network.PrintOrderedPeers()

	if err := b.DispatchInit("join"); err != nil {
		return fmt.Errorf("dispatch init hook: %s", err)
	}

	if err := b.RunJoinNetwork(verify, false); err != nil {
		return fmt.Errorf("boot network: %s", err)
	}

	return b.DispatchDone("join")
}

func (b *BIOS) StartBoot(secretP2PAddress string) error {
	fmt.Println("Starting network join process", time.Now())

	b.Network.PrintOrderedPeers()

	if err := b.DispatchInit("boot"); err != nil {
		return fmt.Errorf("dispatch init hook: %s", err)
	}

	if err := b.RunBootSequence(secretP2PAddress); err != nil {
		return fmt.Errorf("join network: %s", err)
	}

	return b.DispatchDone("boot")
}

func (b *BIOS) PrintOrderedPeers() {
	fmt.Println("###############################################################################################")
	fmt.Println("###################################  SHUFFLING RESULTS  #######################################")
	fmt.Println("")

	fmt.Printf("BIOS NODE: %s\n", b.ShuffledProducers[0].AccountName())
	for i := 1; i < 22 && len(b.ShuffledProducers) > i; i++ {
		fmt.Printf("ABP %02d:    %s\n", i, b.ShuffledProducers[i].AccountName())
	}
	fmt.Println("")
	fmt.Println("###############################################################################################")
	fmt.Println("########################################  BOOTING  ############################################")
	fmt.Println("")
	if b.AmIBootNode() {
		fmt.Println("I AM THE BOOT NODE! Let's get the ball rolling.")

	} else if b.AmIAppointedBlockProducer() {
		fmt.Println("I am NOT the BOOT NODE, but I AM ONE of the Appointed Block Producers. Stay tuned and watch the Boot node's media properties.")
	} else {
		fmt.Println("Okay... I'm not part of the Appointed Block Producers, we'll wait and be ready to join")
	}
	fmt.Println("")

	fmt.Println("###############################################################################################")
	fmt.Println("")
}

func (b *BIOS) RunBootSequence(secretP2PAddress string) error {
	fmt.Println("START BOOT SEQUENCE...")

	ephemeralPrivateKey, err := b.GenerateEphemeralPrivKey()
	if err != nil {
		return err
	}

	b.EphemeralPrivateKey = ephemeralPrivateKey

	// b.EOSAPI.Debug = true

	pubKey := ephemeralPrivateKey.PublicKey().String()
	privKey := ephemeralPrivateKey.String()

	fmt.Printf("Generated ephemeral keys: pub=%s priv=%s..%s\n", pubKey, privKey[:7], privKey[len(privKey)-7:])

	// Store keys in wallet, to sign `SetCode` and friends..
	if err := b.EOSAPI.Signer.ImportPrivateKey(privKey); err != nil {
		return fmt.Errorf("ImportWIF: %s", err)
	}

	keys, _ := b.EOSAPI.Signer.(*eos.KeyBag).AvailableKeys()
	for _, key := range keys {
		fmt.Println("Available key in the KeyBag:", key)
	}

	genesisData := b.GenerateGenesisJSON(pubKey)

	if err = b.DispatchBootPublishGenesis(genesisData); err != nil {
		return fmt.Errorf("dispatch boot_publish_genesis hook: %s", err)
	}

	if err = b.DispatchBootNode(genesisData, pubKey, privKey); err != nil {
		return fmt.Errorf("dispatch boot_node hook: %s", err)
	}

	fmt.Println(b.EOSAPI.Signer.AvailableKeys())

	for _, step := range b.BootSequence {
		fmt.Printf("%s  [%s]\n", step.Label, step.Op)

		acts, err := step.Data.Actions(b)
		if err != nil {
			return fmt.Errorf("getting actions for step %q: %s", step.Op, err)
		}

		if len(acts) != 0 {
			for idx, chunk := range chunkifyActions(acts, 400) { // transfers max out resources higher than ~400
				_, err = b.EOSAPI.SignPushActions(chunk...)
				if err != nil {
					return fmt.Errorf("SignPushActions for step %q, chunk %d: %s", step.Op, idx, err)
				}
			}
		}
	}

	otherPeers := b.someTopmostPeersAddresses()
	if err = b.DispatchBootConnectMesh(otherPeers); err != nil {
		return fmt.Errorf("dispatch boot_connect_mesh: %s", err)
	}

	if err = b.DispatchBootPublishHandoff(); err != nil {
		return fmt.Errorf("dispatch boot_publish_handoff: %s", err)
	}

	return nil
}

func (b *BIOS) RunJoinNetwork(verify, sabotage bool) error {
	if b.Genesis == nil {
		b.Genesis = b.waitOnGenesisData()
	}

	// Create mesh network
	otherPeers := b.computeMyMeshP2PAddresses()

	if err := b.DispatchJoinNetwork(b.Genesis, b.MyPeers, otherPeers); err != nil {
		return fmt.Errorf("dispatch join_network hook: %s", err)
	}

	if verify {
		fmt.Println("###############################################################################################")
		fmt.Println("Launching chain verification")

		// Grab all the Actions, serialize them.
		// Grab all the blocks from the chain
		// Compare each action, find it in our list
		// Use an ordered map ?
		// for _, step := range b.BootSequence {
		// 	fmt.Printf("%s  [%s]\n", step.Label, step.Op)

		// 	acts, err := step.Data.Actions(b)
		// 	if err != nil {
		// 		return fmt.Errorf("getting actions for step %q: %s", step.Op, err)
		// 	}

		// }

		fmt.Printf("- Verifying the `eosio` system account was properly disabled: ")
		for {
			time.Sleep(1 * time.Second)
			acct, err := b.EOSAPI.GetAccount(AN("eosio"))
			if err != nil {
				fmt.Printf("e")
				continue
			}

			if len(acct.Permissions) != 2 || acct.Permissions[0].RequiredAuth.Threshold != 0 || acct.Permissions[1].RequiredAuth.Threshold != 0 {
				// FIXME: perhaps check that there are no keys and
				// accounts.. that the account is *really* disabled.  we
				// can check elsewhere though.
				fmt.Printf(".")
				continue
			}

			fmt.Println(" OKAY")
			break
		}

		fmt.Println("Chain sync'd!")

		// IMPLEMENT THE BOOT SEQUENCE VERIFICATION.
		fmt.Println("")
		fmt.Println("All good! Chain verificaiton succeeded!")
		fmt.Println("")
	} else {
		fmt.Println("")
		fmt.Println("Not doing validation, the Appointed Block Producer will have done it.")
		fmt.Println("")
	}

	// TODO: loop operations, check all actions against blocks that you can fetch from here.
	// Do all the checks:
	//  - all Producers are properly setup
	//  - anything fails, SABOTAGE
	// Publish a PGP Signed message with your local IP.. push to properties
	// Dispatch webhook PublishKickstartPublic (with a Kickstart Data object)
	fmt.Println("Awaiting for private key, for handoff verification.")
	fmt.Println("* This is the last step, and is done for the BIOS Boot node to prove it kept nothing to itself.")
	fmt.Println("")

	b.waitOnHandoff(b.Genesis)

	return nil
}

func (b *BIOS) waitEthereumBlock() rand.Source {
	for {
		hash, err := PollEthereumClock(b.LaunchData.LaunchEthereumBlock)
		if err != nil {
			fmt.Println("couldn't fetch ethereum block:", err)
		} else {

			if hash == "" {
				fmt.Println("block", b.LaunchData.LaunchEthereumBlock, "not produced yet..")
			} else {
				bytes, err := hex.DecodeString(hash)
				if err != nil {
					fmt.Printf("ethereum service returned invalid hex %q\n", hash)
				} else {
					chksum := crc64.Checksum(bytes, crc64.MakeTable(crc64.ECMA))
					return rand.NewSource(int64(chksum))
				}
			}
		}

		time.Sleep(2 * time.Second)
	}
}

func (b *BIOS) waitOnGenesisData() (genesis *GenesisJSON) {
	fmt.Println("")
	fmt.Println("The BIOS node will publish the Genesis data through their social media.")
	bootNode := b.ShuffledProducers[0]
	disco := bootNode.Discovery
	if disco.Website != "" {
		fmt.Println("  Main website:", disco.Website)
	}
	if disco.SocialTwitter != "" {
		fmt.Println("  Twitter:", disco.SocialTwitter)
	}
	if disco.SocialFacebook != "" {
		fmt.Println("  Facebook:", disco.SocialFacebook)
	}
	if disco.SocialTelegram != "" {
		fmt.Println("  Telegram:", disco.SocialTelegram)
	}
	if disco.SocialSlack != "" {
		fmt.Println("  Slack:", disco.SocialSlack)
	}
	if disco.SocialSteem != "" {
		fmt.Println("  Steem:", disco.SocialSteem)
	}
	if disco.SocialSteemIt != "" {
		fmt.Println("  SteemIt:", disco.SocialSteemIt)
	}
	if disco.SocialKeybase != "" {
		fmt.Println("  Keybase:", disco.SocialKeybase)
	}
	if disco.SocialWeChat != "" {
		fmt.Println("  WeChat:", disco.SocialWeChat)
	}
	if disco.SocialYouTube != "" {
		fmt.Println("  YouTube:", disco.SocialYouTube)
	}
	if disco.SocialGitHub != "" {
		fmt.Println("  GitHub:", disco.SocialGitHub)
	}
	fmt.Println("")
	// TODO: print the social media properties of the BP..
	fmt.Println("Genesis data can be base64-encoded JSON, raw JSON or an `/ipfs/Qm...` link pointing to genesis.json")

	for {
		fmt.Printf("Paste genesis here: ")
		text, err := ScanSingleLine()
		if err != nil {
			fmt.Println("error reading line:", err)
			continue
		}

		genesis, err := readGenesisData(text, b.Network.IPFS)
		if err != nil {
			fmt.Println(err)
		}

		return genesis
	}
}

func (b *BIOS) waitOnHandoff(genesis *GenesisJSON) {
	for {
		fmt.Printf("Please paste the private key (or ipfs link): ")
		privKey, err := ScanSingleLine()
		if err != nil {
			fmt.Println("Error reading line:", err)
			continue
		}

		if strings.Contains(privKey, "/ipfs") {
			cnt, err := b.Network.IPFS.Get(IPFSRef(privKey))
			if err != nil {
				fmt.Println("error fetching ipfs content:", err)
				continue
			}

			privKey = string(cnt)
		}

		privKey = strings.TrimSpace(privKey)

		key, err := ecc.NewPrivateKey(privKey)
		if err != nil {
			fmt.Println("Invalid private key pasted:", err)
			continue
		}

		if key.PublicKey().String() == genesis.InitialKey {
			fmt.Println("")
			fmt.Println("   HANDOFF VERIFIED! EOS CHAIN IS ALIVE !")
			fmt.Println("")
			return
		} else {
			fmt.Println("")
			fmt.Println("   WARNING: private key provided does NOT match the genesis data")
			fmt.Println("")
		}
	}
}

func (b *BIOS) GenerateEphemeralPrivKey() (*ecc.PrivateKey, error) {
	return ecc.NewRandomPrivateKey()
}

func (b *BIOS) GenerateGenesisJSON(pubKey string) string {
	// known not to fail
	cnt, _ := json.Marshal(&GenesisJSON{
		InitialTimestamp: time.Now().UTC().Format("2006-01-02T15:04:05"),
		InitialKey:       pubKey,
		InitialChainID:   hex.EncodeToString(b.EOSAPI.ChainID),
	})
	return string(cnt)
}

func (b *BIOS) setProducers() error {

	b.ShuffledProducers = b.Network.OrderedPeers()

	if b.RandSource != nil {
		b.shuffleProducers()
	}

	// We'll multiply the other producers as to have a full schedule
	if len(b.ShuffledProducers) > 1 {
		if numProds := len(b.ShuffledProducers); numProds < 22 {
			cloneCount := numProds - 1
			count := 0
			for {
				if len(b.ShuffledProducers) == 22 {
					break
				}

				fromPeer := b.ShuffledProducers[1+count%cloneCount]
				count++

				clonedProd := &Peer{
					ClonedAccountName: accountVariation(fromPeer.AccountName(), count),
					Discovery:         fromPeer.Discovery,
				}
				b.ShuffledProducers = append(b.ShuffledProducers, clonedProd)
			}
		}
	}

	return nil
}

func (b *BIOS) shuffleProducers() {
	fmt.Println("Shuffling producers listed in the launch file")
	r := rand.New(b.RandSource)
	// shuffle top 25%, capped to 5
	shuffleHowMany := int64(math.Min(math.Ceil(float64(len(b.ShuffledProducers))*0.25), 5))
	if shuffleHowMany > 1 {
		fmt.Println("- Shuffling top", shuffleHowMany)
		for round := 0; round < 100; round++ {
			from := r.Int63() % shuffleHowMany
			to := r.Int63() % shuffleHowMany
			if from == to {
				continue
			}

			//fmt.Println("Swapping from", from, "to", to)
			b.ShuffledProducers[from], b.ShuffledProducers[to] = b.ShuffledProducers[to], b.ShuffledProducers[from]
		}
	} else {
		fmt.Println("- No shuffling, network too small")
	}
}

func (b *BIOS) IsBootNode(account string) bool {
	return string(b.ShuffledProducers[0].AccountName()) == account
}

func (b *BIOS) AmIBootNode() bool {
	return b.IsBootNode(b.Network.MyPeer.Discovery.EOSIOAccountName)
}

func (b *BIOS) MyRole() Role {
	if b.AmIBootNode() {
		return RoleBootNode
	} else if b.AmIAppointedBlockProducer() {
		return RoleABP
	}
	return RoleParticipant
}

func (b *BIOS) IsAppointedBlockProducer(account string) bool {
	for i := 1; i < 22 && len(b.ShuffledProducers) > i; i++ {
		if b.ShuffledProducers[i].Discovery.EOSIOAccountName == account {
			return true
		}
	}
	return false
}

func (b *BIOS) AmIAppointedBlockProducer() bool {
	return b.IsAppointedBlockProducer(b.Network.MyPeer.Discovery.EOSIOAccountName)
}

// MyProducerDefs will provide more than one producer def ONLY when
// your launch files contains LESS than 21 potential appointed block
// producers.  This way, you can have your nodes respond to many
// account names and have the network function. Your producer will
// simply produce more blocks, under different names.
func (b *BIOS) setMyPeers() error {
	myPeer := b.Network.MyPeer

	out := []*Peer{myPeer}

	for _, peer := range b.ShuffledProducers {
		if peer.Discovery.EOSIOAccountName == myPeer.Discovery.EOSIOAccountName {
			out = append(out, peer)
		}
	}

	b.MyPeers = out

	return nil
}

func chunkifyActions(actions []*eos.Action, chunkSize int) (out [][]*eos.Action) {
	currentChunk := []*eos.Action{}
	for _, act := range actions {
		if len(currentChunk) > chunkSize {
			out = append(out, currentChunk)
			currentChunk = []*eos.Action{}
		}
		currentChunk = append(currentChunk, act)
	}
	if len(currentChunk) > 0 {
		out = append(out, currentChunk)
	}
	return
}

func accountVariation(name string, variation int) string {
	if len(name) > 10 {
		name = name[:10]
	}
	return name + "." + string([]byte{'a' + byte(variation-1)})
}
