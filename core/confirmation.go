package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/OpenBazaar/openbazaar-go/pb"
	"github.com/OpenBazaar/spvwallet"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	hd "github.com/btcsuite/btcutil/hdkeychain"
	crypto "gx/ipfs/QmUWER4r4qMvaCnX5zREcfyiWN7cXN9g3a7fkRqNz8qWPP/go-libp2p-crypto"
	"gx/ipfs/QmYf7ng2hG5XBtJA3tN34DQ2GUN5HNksEw1rLDkmr6vGku/go-multihash"
	"gx/ipfs/QmZ4Qi3GaRbjcx28Sme5eMH7RQjGkt8wHxt2a65oLaeFEV/gogo-protobuf/proto"
)

func (n *OpenBazaarNode) NewOrderConfirmation(contract *pb.RicardianContract, addressRequest bool) (*pb.RicardianContract, error) {
	oc := new(pb.OrderConfirmation)
	// Calculate order ID
	orderID, err := n.CalcOrderId(contract.BuyerOrder)
	if err != nil {
		return nil, err
	}
	oc.OrderID = orderID
	if addressRequest {
		addr := n.Wallet.CurrentAddress(spvwallet.EXTERNAL)
		oc.PaymentAddress = addr.EncodeAddress()
	}

	if contract.BuyerOrder.Payment.Method == pb.Order_Payment_MODERATED {
		var slugs []string
		for _, listing := range contract.VendorListings {
			slugs = append(slugs, listing.Slug)
		}
		buyerKey := contract.BuyerOrder.RatingKey
		moderatorKey, err := hex.DecodeString(ExtraModeratorKeyFromReddemScript(contract.BuyerOrder.Payment.RedeemScript))
		if err != nil {
			return nil, err
		}
		metadata := new(pb.RatingSignature_TransactionMetadata)
		metadata.ListingSlugs = slugs
		metadata.ModeratorKey = moderatorKey
		metadata.RatingKey = buyerKey

		ser, err := proto.Marshal(metadata)
		if err != nil {
			return nil, err
		}
		sig, err := n.IpfsNode.PrivateKey.Sign(ser)
		if err != nil {
			return nil, err
		}

		rs := new(pb.RatingSignature)
		rs.Metadata = metadata
		rs.Signature = sig

		oc.RatingSignature = rs
		oc.PaymentAddress = contract.BuyerOrder.Payment.Address
		oc.PayoutFee = n.Wallet.GetFeePerByte(spvwallet.NORMAL)
	}

	oc.RequestedAmount, err = n.CalculateOrderTotal(contract)
	if err != nil {
		return nil, err
	}
	contract.VendorOrderConfirmation = oc
	contract, err = n.SignOrderConfirmation(contract)
	if err != nil {
		return nil, err
	}
	return contract, nil
}

func (n *OpenBazaarNode) ConfirmOfflineOrder(contract *pb.RicardianContract, records []*spvwallet.TransactionRecord) error {
	contract, err := n.NewOrderConfirmation(contract, false)
	if err != nil {
		return err
	}
	if contract.BuyerOrder.Payment.Method != pb.Order_Payment_MODERATED {
		// Sweep the temp address into our wallet
		var utxos []spvwallet.Utxo
		for _, r := range records {
			if !r.Spent && r.Value > 0 {
				u := spvwallet.Utxo{}
				scriptBytes, err := hex.DecodeString(r.ScriptPubKey)
				if err != nil {
					return err
				}
				u.ScriptPubkey = scriptBytes
				hash, err := chainhash.NewHashFromStr(r.Txid)
				if err != nil {
					return err
				}
				outpoint := wire.NewOutPoint(hash, r.Index)
				u.Op = *outpoint
				u.Value = r.Value
				utxos = append(utxos, u)
			}
		}

		chaincode, err := hex.DecodeString(contract.BuyerOrder.Payment.Chaincode)
		if err != nil {
			return err
		}
		parentFP := []byte{0x00, 0x00, 0x00, 0x00}
		mPrivKey := n.Wallet.MasterPrivateKey()
		if err != nil {
			return err
		}
		mECKey, err := mPrivKey.ECPrivKey()
		if err != nil {
			return err
		}
		hdKey := hd.NewExtendedKey(
			n.Wallet.Params().HDPrivateKeyID[:],
			mECKey.Serialize(),
			chaincode,
			parentFP,
			0,
			0,
			true)

		vendorKey, err := hdKey.Child(0)
		if err != nil {
			return err
		}
		redeemScript, err := hex.DecodeString(contract.BuyerOrder.Payment.RedeemScript)
		err = n.Wallet.SweepMultisig(utxos, vendorKey, redeemScript, spvwallet.NORMAL)
		if err != nil {
			return err
		}
	}
	err = n.SendOrderConfirmation(contract.BuyerOrder.BuyerID.Guid, contract)
	if err != nil {
		return err
	}
	n.Datastore.Sales().Put(contract.VendorOrderConfirmation.OrderID, *contract, pb.OrderState_FUNDED, false)
	return nil
}

func (n *OpenBazaarNode) RejectOfflineOrder(contract *pb.RicardianContract, records []*spvwallet.TransactionRecord) error {
	orderId, err := n.CalcOrderId(contract.BuyerOrder)
	if err != nil {
		return err
	}
	rejectMsg := new(pb.OrderReject)
	rejectMsg.OrderID = orderId
	if contract.BuyerOrder.Payment.Method == pb.Order_Payment_MODERATED {
		var ins []spvwallet.TransactionInput
		var outValue int64
		for _, r := range records {
			if !r.Spent && r.Value > 0 {
				outpointHash, err := hex.DecodeString(r.Txid)
				if err != nil {
					return err
				}
				outValue += r.Value
				in := spvwallet.TransactionInput{OutpointIndex: r.Index, OutpointHash: outpointHash}
				ins = append(ins, in)
			}
		}

		refundAddress, err := btcutil.DecodeAddress(contract.BuyerOrder.RefundAddress, n.Wallet.Params())
		if err != nil {
			return err
		}
		var output spvwallet.TransactionOutput

		outputScript, err := txscript.PayToAddrScript(refundAddress)
		if err != nil {
			return err
		}
		output.ScriptPubKey = outputScript
		output.Value = outValue

		chaincode, err := hex.DecodeString(contract.BuyerOrder.Payment.Chaincode)
		if err != nil {
			return err
		}
		parentFP := []byte{0x00, 0x00, 0x00, 0x00}
		mPrivKey := n.Wallet.MasterPrivateKey()
		if err != nil {
			return err
		}
		mECKey, err := mPrivKey.ECPrivKey()
		if err != nil {
			return err
		}
		hdKey := hd.NewExtendedKey(
			n.Wallet.Params().HDPrivateKeyID[:],
			mECKey.Serialize(),
			chaincode,
			parentFP,
			0,
			0,
			true)

		vendorKey, err := hdKey.Child(0)
		if err != nil {
			return err
		}
		redeemScript, err := hex.DecodeString(contract.BuyerOrder.Payment.RedeemScript)

		signatures, err := n.Wallet.CreateMultisigSignature(ins, []spvwallet.TransactionOutput{output}, vendorKey, redeemScript, contract.BuyerOrder.RefundFee)
		if err != nil {
			return err
		}
		var sigs []*pb.BitcoinSignature
		for _, s := range signatures {
			pbSig := &pb.BitcoinSignature{Signature: s.Signature, InputIndex: s.InputIndex}
			sigs = append(sigs, pbSig)
		}
		rejectMsg.Sigs = sigs
	}
	err = n.SendReject(contract.BuyerOrder.BuyerID.Guid, rejectMsg)
	if err != nil {
		return err
	}
	n.Datastore.Sales().Put(orderId, *contract, pb.OrderState_REJECTED, true)
	return nil
}

func (n *OpenBazaarNode) ValidateOrderConfirmation(contract *pb.RicardianContract, validateAddress bool) error {
	orderID, err := n.CalcOrderId(contract.BuyerOrder)
	if err != nil {
		return err
	}
	if contract.VendorOrderConfirmation.OrderID != orderID {
		return errors.New("Vendor's response contained invalid order ID")
	}
	if contract.VendorOrderConfirmation.RequestedAmount != contract.BuyerOrder.Payment.Amount {
		return errors.New("Vendor requested an amount different from what we calculated")
	}
	if contract.BuyerOrder.Payment.Method == pb.Order_Payment_MODERATED {
		var slugs []string
		for _, listing := range contract.VendorListings {
			slugs = append(slugs, listing.Slug)
		}
		matches := 0
	outer:
		for _, slug := range slugs {
			for _, mSlug := range contract.VendorOrderConfirmation.RatingSignature.Metadata.ListingSlugs {
				if mSlug == slug {
					matches++
					continue outer
				}
			}
		}
		if matches != len(slugs) {
			return errors.New("Rating signatures do not cover all listings")
		}
		pubkey, err := crypto.UnmarshalPublicKey(contract.VendorListings[0].VendorID.Pubkeys.Guid)
		if err != nil {
			return err
		}
		moderatorKey, err := hex.DecodeString(ExtraModeratorKeyFromReddemScript(contract.BuyerOrder.Payment.RedeemScript))
		if err != nil {
			return err
		}
		if !bytes.Equal(contract.VendorOrderConfirmation.RatingSignature.Metadata.RatingKey, contract.BuyerOrder.RatingKey) {
			return errors.New("Rating signature does not contian rating key")
		}
		if !bytes.Equal(contract.VendorOrderConfirmation.RatingSignature.Metadata.ModeratorKey, moderatorKey) {
			return errors.New("Rating signature does not contain moderatory key")
		}
		ser, err := proto.Marshal(contract.VendorOrderConfirmation.RatingSignature.Metadata)
		if err != nil {
			return err
		}
		valid, err := pubkey.Verify(ser, contract.VendorOrderConfirmation.RatingSignature.Signature)
		if err != nil || !valid {
			return errors.New("Failed to verify signature on rating keys")
		}
	}
	if validateAddress {
		_, err = btcutil.DecodeAddress(contract.VendorOrderConfirmation.PaymentAddress, n.Wallet.Params())
		if err != nil {
			return err
		}
	}
	err = verifySignaturesOnOrderConfirmation(contract)
	if err != nil {
		return err
	}
	return nil
}

func (n *OpenBazaarNode) SignOrderConfirmation(contract *pb.RicardianContract) (*pb.RicardianContract, error) {
	serializedOrderConf, err := proto.Marshal(contract.VendorOrderConfirmation)
	if err != nil {
		return contract, err
	}
	s := new(pb.Signatures)
	s.Section = pb.Signatures_ORDER_CONFIRMATION
	if err != nil {
		return contract, err
	}
	guidSig, err := n.IpfsNode.PrivateKey.Sign(serializedOrderConf)
	if err != nil {
		return contract, err
	}
	priv, err := n.Wallet.MasterPrivateKey().ECPrivKey()
	if err != nil {
		return contract, err
	}
	hashed := sha256.Sum256(serializedOrderConf)
	bitcoinSig, err := priv.Sign(hashed[:])
	if err != nil {
		return contract, err
	}
	s.Guid = guidSig
	s.Bitcoin = bitcoinSig.Serialize()
	contract.Signatures = append(contract.Signatures, s)
	return contract, nil
}

func verifySignaturesOnOrderConfirmation(contract *pb.RicardianContract) error {
	guidPubkeyBytes := contract.VendorListings[0].VendorID.Pubkeys.Guid
	bitcoinPubkeyBytes := contract.VendorListings[0].VendorID.Pubkeys.Bitcoin
	guid := contract.VendorListings[0].VendorID.Guid
	ser, err := proto.Marshal(contract.VendorOrderConfirmation)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(ser)
	guidPubkey, err := crypto.UnmarshalPublicKey(guidPubkeyBytes)
	if err != nil {
		return err
	}
	bitcoinPubkey, err := btcec.ParsePubKey(bitcoinPubkeyBytes, btcec.S256())
	if err != nil {
		return err
	}
	var guidSig []byte
	var bitcoinSig *btcec.Signature
	var sig *pb.Signatures
	sigExists := false
	for _, s := range contract.Signatures {
		if s.Section == pb.Signatures_ORDER_CONFIRMATION {
			sig = s
			sigExists = true
			break
		}
	}
	if !sigExists {
		return errors.New("Contract does not contain a signature for the order confirmation")
	}
	guidSig = sig.Guid
	bitcoinSig, err = btcec.ParseSignature(sig.Bitcoin, btcec.S256())
	if err != nil {
		return err
	}
	valid, err := guidPubkey.Verify(ser, guidSig)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("Vendor's guid signature on contact failed to verify")
	}
	checkKeyHash, err := guidPubkey.Hash()
	if err != nil {
		return err
	}
	guidMH, err := multihash.FromB58String(guid)
	if err != nil {
		return err
	}
	if !bytes.Equal(guidMH, checkKeyHash) {
		return errors.New("Public key in order does not match reported vendor ID")
	}
	valid = bitcoinSig.Verify(hash[:], bitcoinPubkey)
	if !valid {
		return errors.New("Vendors's bitcoin signature on contact failed to verify")
	}

	return nil
}

func ExtraModeratorKeyFromReddemScript(redeemScript string) string {
	return redeemScript[134:200]
}