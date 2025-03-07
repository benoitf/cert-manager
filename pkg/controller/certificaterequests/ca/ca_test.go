/*
Copyright 2019 The Jetstack cert-manager contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ca

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/jetstack/cert-manager/pkg/apis/certmanager"
	"github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha1"
	testpkg "github.com/jetstack/cert-manager/pkg/controller/test"
	"github.com/jetstack/cert-manager/pkg/issuer"
	"github.com/jetstack/cert-manager/pkg/util/pki"
	"github.com/jetstack/cert-manager/test/unit/gen"
)

func generateRSAPrivateKey(t *testing.T) *rsa.PrivateKey {
	pk, err := pki.GenerateRSAPrivateKey(2048)
	if err != nil {
		t.Errorf("failed to generate private key: %v", err)
		t.FailNow()
	}
	return pk
}

func generateCSR(t *testing.T, secretKey crypto.Signer) ([]byte, error) {
	asn1Subj, _ := asn1.Marshal(pkix.Name{
		CommonName: "test",
	}.ToRDNSequence())
	template := x509.CertificateRequest{
		RawSubject:         asn1Subj,
		SignatureAlgorithm: x509.SHA256WithRSA,
	}

	csrBytes, err := x509.CreateCertificateRequest(rand.Reader, &template, secretKey)
	if err != nil {
		return nil, err
	}

	csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrBytes})

	return csr, nil
}

func generateSelfSignedCertFromCR(t *testing.T, cr *v1alpha1.CertificateRequest, key crypto.Signer,
	duration time.Duration) (derBytes, pemBytes []byte) {
	template, err := pki.GenerateTemplateFromCertificateRequest(cr)
	if err != nil {
		t.Errorf("error generating template: %v", err)
	}

	derBytes, err = x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Errorf("error signing cert: %v", err)
		t.FailNow()
	}

	pemByteBuffer := bytes.NewBuffer([]byte{})
	err = pem.Encode(pemByteBuffer, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	if err != nil {
		t.Errorf("failed to encode cert: %v", err)
		t.FailNow()
	}

	return derBytes, pemByteBuffer.Bytes()
}

func mustNoResponse(builder *testpkg.Builder, args ...interface{}) {
	resp := args[0].(*issuer.IssueResponse)
	if resp != nil {
		builder.T.Errorf("unexpected response, exp='nil' got='%+v'", resp)
	}
}

func noPrivateKeyFieldsSetCheck(expectedCA []byte) func(builder *testpkg.Builder, args ...interface{}) {
	return func(builder *testpkg.Builder, args ...interface{}) {
		resp := args[0].(*issuer.IssueResponse)

		if resp == nil {
			builder.T.Errorf("no response given, got=%s", resp)
			return
		}

		if len(resp.PrivateKey) > 0 {
			builder.T.Errorf("expected no new private key to be generated but got: %s",
				resp.PrivateKey)
		}

		certificatesFieldsSetCheck(expectedCA)(builder, args...)
	}
}

func certificatesFieldsSetCheck(expectedCA []byte) func(builder *testpkg.Builder, args ...interface{}) {
	return func(builder *testpkg.Builder, args ...interface{}) {
		resp := args[0].(*issuer.IssueResponse)

		if resp.Certificate == nil {
			builder.T.Errorf("expected new certificate to be issued")
		}
		if resp.CA == nil || !reflect.DeepEqual(expectedCA, resp.CA) {
			builder.T.Errorf("expected CA certificate to be returned")
		}
	}
}

func TestSign(t *testing.T) {
	// Build root RSA CA
	rsaPK := generateRSAPrivateKey(t)
	rsaPKBytes := pki.EncodePKCS1PrivateKey(rsaPK)

	caCSR, err := generateCSR(t, rsaPK)
	if err != nil {
		t.Errorf("failed to generate CA CSR: %s", err)
		t.FailNow()
	}

	rootRSACR := gen.CertificateRequest("test-root-ca",
		gen.SetCertificateRequestCSR(caCSR),
		gen.SetCertificateRequestIsCA(true),
		gen.SetCertificateRequestDuration(&metav1.Duration{Duration: time.Hour * 24 * 60}),
	)

	// generate a self signed root ca valid for 60d
	_, rsaPEMCert := generateSelfSignedCertFromCR(t, rootRSACR, rsaPK, time.Hour*24*60)
	rootRSACASecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "root-ca-secret",
			Namespace: gen.DefaultTestNamespace,
		},
		Data: map[string][]byte{
			corev1.TLSPrivateKeyKey: rsaPKBytes,
			corev1.TLSCertKey:       rsaPEMCert,
		},
	}

	rootRSANoCASecret := rootRSACASecret.DeepCopy()
	rootRSANoCASecret.Data[corev1.TLSCertKey] = make([]byte, 0)
	rootRSANoKeySecret := rootRSACASecret.DeepCopy()
	rootRSANoKeySecret.Data[corev1.TLSPrivateKeyKey] = make([]byte, 0)

	tests := map[string]testT{
		"sign a CertificateRequest": {
			certificateRequest: gen.CertificateRequest("test-cr",
				gen.SetCertificateRequestIsCA(true),
				gen.SetCertificateRequestCSR(caCSR),
				gen.SetCertificateRequestIssuer(v1alpha1.ObjectReference{
					Name:  "ca-issuer",
					Group: certmanager.GroupName,
					Kind:  "Issuer",
				}),
			),
			builder: &testpkg.Builder{
				KubeObjects: []runtime.Object{rootRSACASecret},
				CertManagerObjects: []runtime.Object{
					gen.Issuer("ca-issuer",
						gen.SetIssuerCA(v1alpha1.CAIssuer{SecretName: "root-ca-secret"}),
					),
				},
				// we are not expecting key on response
				CheckFn: noPrivateKeyFieldsSetCheck(rsaPEMCert),
			},
		},
		"fail to find CA tls key pair": {
			certificateRequest: gen.CertificateRequest("test-cr",
				gen.SetCertificateRequestIsCA(true),
				gen.SetCertificateRequestCSR(caCSR),
				gen.SetCertificateRequestIssuer(v1alpha1.ObjectReference{
					Name:  "ca-issuer",
					Group: certmanager.GroupName,
					Kind:  "Issuer",
				}),
			),
			builder: &testpkg.Builder{
				KubeObjects: []runtime.Object{},
				CertManagerObjects: []runtime.Object{gen.Issuer("ca-issuer",
					gen.SetIssuerCA(v1alpha1.CAIssuer{SecretName: "root-ca-secret"}),
				)},
				ExpectedEvents: []string{
					`Warning Pending secret "root-ca-secret" not found`,
				},
				CheckFn: mustNoResponse,
			},
		},
		"given bad CSR should fail Certificate generation": {
			certificateRequest: gen.CertificateRequest("test-cr",
				gen.SetCertificateRequestIsCA(true),
				gen.SetCertificateRequestCSR([]byte("bad-csr")),
				gen.SetCertificateRequestIssuer(v1alpha1.ObjectReference{
					Name:  "ca-issuer",
					Group: certmanager.GroupName,
					Kind:  "Issuer",
				}),
			),
			builder: &testpkg.Builder{
				KubeObjects: []runtime.Object{rootRSACASecret},
				CertManagerObjects: []runtime.Object{gen.Issuer("ca-issuer",
					gen.SetIssuerCA(v1alpha1.CAIssuer{SecretName: "root-ca-secret"}),
				)},
				ExpectedEvents: []string{
					`Warning ErrorSigning Error generating certificate template: failed to decode csr from certificate request resource default-unit-test-ns/test-cr`,
				},
				CheckFn: mustNoResponse,
			},
		},
		"no CA certificate should fail a signing": {
			certificateRequest: gen.CertificateRequest("test-cr",
				gen.SetCertificateRequestIsCA(true),
				gen.SetCertificateRequestCSR(caCSR),
				gen.SetCertificateRequestIssuer(v1alpha1.ObjectReference{
					Name:  "ca-issuer",
					Group: certmanager.GroupName,
					Kind:  "Issuer",
				}),
			),
			builder: &testpkg.Builder{
				KubeObjects: []runtime.Object{rootRSANoCASecret},
				CertManagerObjects: []runtime.Object{gen.Issuer("ca-issuer",
					gen.SetIssuerCA(v1alpha1.CAIssuer{SecretName: "root-ca-secret"}),
				)},
				CheckFn: func(builder *testpkg.Builder, args ...interface{}) {
					err := args[1].(error)
					badCAError := `error decoding cert PEM block`
					if err == nil || err.Error() != badCAError {
						t.Errorf("unexpected error, exp='%s' got='%+v'", badCAError, err)
					}
					mustNoResponse(builder, args...)
				},
			},
			expectedErr: true,
		},
		"no CA key should fail a signing": {
			certificateRequest: gen.CertificateRequest("test-cr",
				gen.SetCertificateRequestIsCA(true),
				gen.SetCertificateRequestCSR(caCSR),
				gen.SetCertificateRequestIssuer(v1alpha1.ObjectReference{
					Name:  "ca-issuer",
					Group: certmanager.GroupName,
					Kind:  "Issuer",
				}),
			),
			builder: &testpkg.Builder{
				KubeObjects: []runtime.Object{rootRSANoKeySecret},
				CertManagerObjects: []runtime.Object{gen.Issuer("ca-issuer",
					gen.SetIssuerCA(v1alpha1.CAIssuer{SecretName: "root-ca-secret"}),
				)},
				CheckFn: func(builder *testpkg.Builder, args ...interface{}) {
					err := args[1].(error)
					noKeyError := "error decoding private key PEM block"
					if err == nil || err.Error() != noKeyError {
						builder.T.Errorf("unexpected error, exp='%s' got='%+v'", noKeyError, err)
					}

					mustNoResponse(builder, args...)
				},
			},
			expectedErr: true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			runTest(t, test)
		})
	}
}

type testT struct {
	builder            *testpkg.Builder
	certificateRequest *v1alpha1.CertificateRequest

	checkFn     func(*testpkg.Builder, ...interface{})
	expectedErr bool
}

func runTest(t *testing.T, test testT) {
	test.builder.T = t
	test.builder.Start()
	defer test.builder.Stop()

	c := NewCA(test.builder.Context)
	test.builder.Sync()

	resp, err := c.Sign(context.Background(), test.certificateRequest)
	if err != nil && !test.expectedErr {
		t.Errorf("expected to not get an error, but got: %v", err)
	}
	if err == nil && test.expectedErr {
		t.Errorf("expected to get an error but did not get one")
	}
	test.builder.CheckAndFinish(resp, err)
}
