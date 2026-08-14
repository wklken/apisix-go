package saml_auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
)

func TestValidateSignedSAMLXMLAcceptsValidSignedLogoutResponse(t *testing.T) {
	cfg := testConfig(t)
	idp := testSAMLSigner(t, cfg.IDPURI, cfg.SPIssuer, cfg.IDPCert, cfg.SPPrivateKey)

	response, err := idp.MakeLogoutResponse(cfg.LogoutCallbackURI, "request-1")
	if err != nil {
		t.Fatalf("MakeLogoutResponse() error = %v", err)
	}
	responseXML, err := samlElementBytes(response.Element())
	if err != nil {
		t.Fatalf("samlElementBytes() error = %v", err)
	}

	validated, err := validateSignedSAMLXML(responseXML, cfg.IDPCert)
	if err != nil {
		t.Fatalf("validateSignedSAMLXML(valid) error = %v", err)
	}
	if !strings.Contains(string(validated), "request-1") {
		t.Fatalf("validated XML = %q, want InResponseTo request-1", validated)
	}
}

func TestValidateSignedSAMLXMLRejectsTwoReferenceSignatureBypass(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(1 * time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))

	doc := etree.NewDocument()
	root := doc.CreateElement("Root")
	root.CreateAttr("ID", "target")
	root.SetText("Malicious Content")

	tlsCert := tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key}
	signingCtx := dsig.NewDefaultSigningContext(dsig.TLSCertKeyStore(tlsCert))

	sig, err := signingCtx.ConstructSignature(root, true)
	if err != nil {
		t.Fatalf("ConstructSignature(root) error = %v", err)
	}

	signedInfo := sig.FindElement("./SignedInfo")
	existingRef := signedInfo.FindElement("./Reference")
	existingRef.CreateAttr("URI", "#dummy")

	originalEl := etree.NewElement("Root")
	originalEl.CreateAttr("ID", "target")
	originalEl.SetText("Original Content")

	sig1, err := signingCtx.ConstructSignature(originalEl, true)
	if err != nil {
		t.Fatalf("ConstructSignature(original) error = %v", err)
	}
	ref1 := sig1.FindElement("./SignedInfo/Reference").Copy()

	signedInfo.InsertChildAt(existingRef.Index(), ref1)

	detachedSI := signedInfo.Copy()
	if detachedSI.SelectAttr("xmlns:"+dsig.DefaultPrefix) == nil {
		detachedSI.CreateAttr("xmlns:"+dsig.DefaultPrefix, dsig.Namespace)
	}
	canonicalBytes, err := signingCtx.Canonicalizer.Canonicalize(detachedSI)
	if err != nil {
		t.Fatalf("canonicalize SignedInfo: %v", err)
	}
	hash := signingCtx.Hash.New()
	hash.Write(canonicalBytes)
	rawSig, err := rsa.SignPKCS1v15(rand.Reader, key, signingCtx.Hash, hash.Sum(nil))
	if err != nil {
		t.Fatalf("sign SignedInfo: %v", err)
	}
	sig.FindElement("./SignatureValue").SetText(base64.StdEncoding.EncodeToString(rawSig))

	root.AddChild(sig)
	doc.SetRoot(root)
	forgedXML, err := doc.WriteToBytes()
	if err != nil {
		t.Fatalf("serialize forged document: %v", err)
	}

	if _, err := validateSignedSAMLXML(forgedXML, certPEM); err == nil {
		t.Fatal("validateSignedSAMLXML accepted forged content")
	}
}
