// Copyright IBM Corp. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"

	util "github.com/hyperledger/fabric/common/util" //JCS import utils

	"github.com/hyperledger/fabric/common/crypto"
	"github.com/hyperledger/fabric/common/localmsp"
	"github.com/hyperledger/fabric/common/tools/protolator"
	mspmgmt "github.com/hyperledger/fabric/msp/mgmt"
	"github.com/hyperledger/fabric/orderer/common/localconfig"
	cb "github.com/hyperledger/fabric/protos/common"
	ab "github.com/hyperledger/fabric/protos/orderer"
	"github.com/hyperledger/fabric/protos/utils"
	"google.golang.org/grpc"
)

var (
	verify         bool    = false                           //JCS: verify signatures?
	blocksReceived int64   = 0                               //JCS: block counter and checker
	N              int64   = 4                               //JCS: number of ordering nodes
	F              int64   = 1                               //JCS: number of faults
	Q              float64 = ((float64(N) + float64(F)) / 2) //JCS: quorum size

	oldest  = &ab.SeekPosition{Type: &ab.SeekPosition_Oldest{Oldest: &ab.SeekOldest{}}}
	newest  = &ab.SeekPosition{Type: &ab.SeekPosition_Newest{Newest: &ab.SeekNewest{}}}
	maxStop = &ab.SeekPosition{Type: &ab.SeekPosition_Specified{Specified: &ab.SeekSpecified{Number: math.MaxUint64}}}
)

type deliverClient struct {
	client    ab.AtomicBroadcast_DeliverClient
	channelID string
	signer    crypto.LocalSigner
	quiet     bool
}

func newDeliverClient(client ab.AtomicBroadcast_DeliverClient, channelID string, signer crypto.LocalSigner, quiet bool) *deliverClient {
	return &deliverClient{client: client, channelID: channelID, signer: signer, quiet: quiet}
}

func (r *deliverClient) seekHelper(start *ab.SeekPosition, stop *ab.SeekPosition) *cb.Envelope {
	env, err := utils.CreateSignedEnvelope(cb.HeaderType_DELIVER_SEEK_INFO, r.channelID, r.signer, &ab.SeekInfo{
		Start:    start,
		Stop:     stop,
		Behavior: ab.SeekInfo_BLOCK_UNTIL_READY,
	}, 0, 0)
	if err != nil {
		panic(err)
	}
	return env
}

func (r *deliverClient) seekOldest() error {
	return r.client.Send(r.seekHelper(oldest, maxStop))
}

func (r *deliverClient) seekNewest() error {
	return r.client.Send(r.seekHelper(newest, maxStop))
}

func (r *deliverClient) seekSingle(blockNumber uint64) error {
	specific := &ab.SeekPosition{Type: &ab.SeekPosition_Specified{Specified: &ab.SeekSpecified{Number: blockNumber}}}
	return r.client.Send(r.seekHelper(specific, specific))
}

func (r *deliverClient) readUntilClose() {
	for {
		msg, err := r.client.Recv()
		if err != nil {
			fmt.Println("Error receiving:", err)
			return
		}

		switch t := msg.Type.(type) {
		case *ab.DeliverResponse_Status:
			fmt.Println("Got status ", t)
			return
		case *ab.DeliverResponse_Block:

			blocksReceived++ //JCS: block count

			if !r.quiet {
				fmt.Println("Received block: ")
				err := protolator.DeepMarshalJSON(os.Stdout, t.Block)
				if err != nil {
					fmt.Printf("  Error pretty printing block: %s", err)
				}
			} else {
				fmt.Println("Received block: ", t.Block.Header.Number)
			}

			if t.Block.Header.Number > 0 && verify { //JCS: check orderer signatures

				meta, _ := utils.UnmarshalMetadata(t.Block.Metadata.Metadata[cb.BlockMetadataIndex_SIGNATURES])

				// JCS: see what the bytes are and compare to proxy
				fmt.Printf("Block #%d contains %d block signatures\n", t.Block.Header.Number, len(meta.Signatures))

				validateSignatures(meta, t.Block)

				meta, _ = utils.UnmarshalMetadata(t.Block.Metadata.Metadata[cb.BlockMetadataIndex_LAST_CONFIG])

				// JCS: see what the bytes are and compare to proxy
				fmt.Printf("Block #%d contains %d lastconfig signatures\n", t.Block.Header.Number, len(meta.Signatures))

				validateSignatures(meta, t.Block)

				fmt.Printf("Blocks received: %d\n", blocksReceived)
			}
		}
	}
}

func main() {
	conf, err := localconfig.Load()
	if err != nil {
		fmt.Println("failed to load config:", err)
		os.Exit(1)
	}

	// Load local MSP
	err = mspmgmt.LoadLocalMsp(conf.General.LocalMSPDir, conf.General.BCCSP, conf.General.LocalMSPID)
	if err != nil { // Handle errors reading the config file
		fmt.Println("Failed to initialize local MSP:", err)
		os.Exit(0)
	}

	signer := localmsp.NewSigner()

	var channelID string
	var serverAddr string
	var seek int
	var quiet bool

	flag.StringVar(&serverAddr, "server", fmt.Sprintf("%s:%d", conf.General.ListenAddress, conf.General.ListenPort), "The RPC server to connect to.")
	flag.StringVar(&channelID, "channelID", localconfig.Defaults.General.SystemChannel, "The channel ID to deliver from.")
	flag.BoolVar(&quiet, "quiet", false, "Only print the block number, will not attempt to print its block contents.")
	flag.IntVar(&seek, "seek", -2, "Specify the range of requested blocks."+
		"Acceptable values:"+
		"-2 (or -1) to start from oldest (or newest) and keep at it indefinitely."+
		"N >= 0 to fetch block N only.")

	//JCS: my new flags
	flag.Int64Var(&N, "n", N, "The total number of ordering nodes operating in the system.")
	flag.Int64Var(&F, "f", F, "The number of Byzantine ordering nodes that are being tolerated.")
	flag.BoolVar(&verify, "verify", verify, "Verify block signatures.")

	flag.Parse()

	if seek < -2 {
		fmt.Println("Wrong seek value.")
		flag.PrintDefaults()
	}

	conn, err := grpc.Dial(serverAddr, grpc.WithInsecure())
	if err != nil {
		fmt.Println("Error connecting:", err)
		return
	}
	client, err := ab.NewAtomicBroadcastClient(conn).Deliver(context.TODO())
	if err != nil {
		fmt.Println("Error connecting:", err)
		return
	}

	s := newDeliverClient(client, channelID, signer, quiet)
	switch seek {
	case -2:
		err = s.seekOldest()
	case -1:
		err = s.seekNewest()
	default:
		err = s.seekSingle(uint64(seek))
	}

	if err != nil {
		fmt.Println("Received error:", err)
	}

	s.readUntilClose()
}

func validateSignatures(meta *cb.Metadata, block *cb.Block) { //JCS: function to validate ordering nodes signatures

	if block.Header.Number == 0 {
		fmt.Printf("Block #0 requires no signature validation!\n")
		return
	}

	des := mspmgmt.GetIdentityDeserializer("")
	validSigs := int64(0)

	for i, sig := range meta.Signatures {

		bytes := util.ConcatenateBytes(meta.Value, sig.SignatureHeader, block.Header.Bytes())

		sigHeader, err := utils.UnmarshalSignatureHeader(sig.SignatureHeader)
		if err != nil {
			fmt.Println("Signature Header Problem: ", err)
			continue
		}
		ident, err := des.DeserializeIdentity(sigHeader.Creator)
		if err != nil {
			fmt.Println("Identity Problem: ", err)
			continue
		}

		fmt.Printf("Signature: #%d\n", i)
		fmt.Printf("MSPID: %s\n", ident.GetMSPIdentifier())
		fmt.Printf("Bytes: %x\n", sig.Signature)

		err = ident.Verify(bytes, sig.Signature)
		if err != nil {
			fmt.Printf("Sig verification problem: %s\n", err)
			continue
		}

		validSigs++

	}

	switch {
	case float64(validSigs) > Q:
		{
			fmt.Printf("Block #%d contains a quorum of valid signatures!\n", block.Header.Number)
		}
	case validSigs > F:
		{
			fmt.Printf("Block #%d contains enough valid signatures...\n", block.Header.Number)
		}
	default:
		{
			fmt.Printf("Block #%d does NOT contain enough valid signatures!\n", block.Header.Number)
		}
	}
}
