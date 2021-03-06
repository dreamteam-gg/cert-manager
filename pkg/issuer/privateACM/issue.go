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

package privateACM

import (
	"bytes"
	"context"
	"encoding/pem"
	"fmt"
	"math"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/defaults"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/acmpca"
	"github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha1"
	"github.com/jetstack/cert-manager/pkg/issuer"
	"github.com/jetstack/cert-manager/pkg/util/errors"
	"github.com/jetstack/cert-manager/pkg/util/kube"
	"github.com/jetstack/cert-manager/pkg/util/pki"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog"
)

func (a *PrivateACM) Issue(ctx context.Context, crt *v1alpha1.Certificate) (*issuer.IssueResponse, error) {
	// get a copy of the existing/currently issued Certificate's private key
	signeeKey, err := kube.SecretTLSKey(ctx, a.secretsLister, crt.Namespace, crt.Spec.SecretName)
	if k8sErrors.IsNotFound(err) || errors.IsInvalidData(err) {
		// if one does not already exist, generate a new one
		signeeKey, err = pki.GeneratePrivateKeyForCertificate(crt)
		if err != nil {
			a.Recorder.Eventf(crt, corev1.EventTypeWarning, "PrivateKeyError", "Error generating certificate private key: %v", err)
			// don't trigger a retry. An error from this function implies some
			// invalid input parameters, and retrying without updating the
			// resource will not help.
			return nil, nil
		}
		klog.V(4).Infof("Storing new certificate private key for %s/%s", crt.Namespace, crt.Name)
		a.Recorder.Eventf(crt, corev1.EventTypeNormal, "Generated", "Generated new private key")

		keyPem, err := pki.EncodePrivateKey(signeeKey, crt.Spec.KeyEncoding)
		if err != nil {
			return nil, err
		}

		// Replace the existing secret with one containing only the new private key.
		return &issuer.IssueResponse{
			PrivateKey: keyPem,
		}, nil
	}
	if err != nil {
		klog.Errorf("Error getting private key %q for certificate: %v", crt.Spec.SecretName, err)
		return nil, err
	}

	/// BEGIN building CSR
	// TODO: we should probably surface some of these errors to users
	template, err := pki.GenerateCSR(crt)
	if err != nil {
		return nil, err
	}

	derBytes, err := pki.EncodeCSR(template, signeeKey)
	if err != nil {
		return nil, err
	}

	pemRequestBuf := &bytes.Buffer{}

	err = pem.Encode(pemRequestBuf, &pem.Block{Type: "CERTIFICATE REQUEST", Bytes: derBytes})
	if err != nil {
		return nil, err
	}

	pca, err := a.initAWSPCAClient()
	if err != nil {
		a.Recorder.Eventf(crt, corev1.EventTypeWarning, "ErrorSigning", "Failed to request certificate: %v", err)
		return nil, err
	}
	/// END building CSR

	/// BEGIN requesting certificate
	certDuration := v1alpha1.DefaultCertificateDuration
	if crt.Spec.Duration != nil {
		certDuration = crt.Spec.Duration.Duration
	}

	issueCertInput := &acmpca.IssueCertificateInput{
		CertificateAuthorityArn: aws.String(a.issuer.GetSpec().PrivateACM.CertificateAuthorityARN),
		Csr:                     pemRequestBuf.Bytes(),
		IdempotencyToken:        aws.String(string(crt.UID)),
		SigningAlgorithm:        aws.String(acmpca.SigningAlgorithmSha256withrsa), // TODO: make this configurable
		Validity: &acmpca.Validity{
			Type:  aws.String("DAYS"),
			Value: aws.Int64(int64(math.Ceil(certDuration.Hours() / 24))),
		},
	}
	issueCertOutput, err := pca.IssueCertificate(issueCertInput)
	if err != nil {
		a.Recorder.Eventf(crt, corev1.EventTypeWarning, "ErrorSigning", "Failed to request certificate: %v", err)
		return nil, err
	}
	a.Recorder.Eventf(crt, corev1.EventTypeNormal, "Requested", "Certificate Requested, ARN: %v", *issueCertOutput.CertificateArn)

	certOutput, err := pca.GetCertificate(&acmpca.GetCertificateInput{
		CertificateAuthorityArn: aws.String(a.issuer.GetSpec().PrivateACM.CertificateAuthorityARN),
		CertificateArn:          issueCertOutput.CertificateArn,
	})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok && awsErr.Code() == acmpca.ErrCodeRequestInProgressException {
			a.Recorder.Eventf(crt, corev1.EventTypeNormal, "InProgress", "Certificate Request is still in progress")
			return nil, fmt.Errorf("certificate Request is still in progress")
		}

		a.Recorder.Eventf(crt, corev1.EventTypeWarning, "ErrorSigning", "Failed to get certificate: %v", err)
		return nil, err
	}

	certificate := []byte(*certOutput.Certificate)

	// Encode output private key and CA cert ready for return
	keyPem, err := pki.EncodePrivateKey(signeeKey, crt.Spec.KeyEncoding)
	if err != nil {
		a.Recorder.Eventf(crt, corev1.EventTypeWarning, "ErrorPrivateKey", "Error encoding private key: %v", err)
		return nil, err
	}

	caCertificateOutput, err := pca.GetCertificateAuthorityCertificate(&acmpca.GetCertificateAuthorityCertificateInput{
		CertificateAuthorityArn: aws.String(a.issuer.GetSpec().PrivateACM.CertificateAuthorityARN),
	})
	if err != nil {
		klog.Errorf("Error getting PCA Certificate: %v", err)
		return nil, err
	}
	caCertificate := []byte(fmt.Sprintf("%s\n%s", *caCertificateOutput.Certificate, *caCertificateOutput.CertificateChain))

	return &issuer.IssueResponse{
		PrivateKey:  keyPem,
		Certificate: certificate,
		CA:          caCertificate,
	}, nil
}

func (a *PrivateACM) initAWSPCAClient() (*acmpca.ACMPCA, error) {
	var accessKeyID, secretKeyID string
	var err error

	if a.issuer.GetSpec().PrivateACM.AccessKeyIDRef.Name != "" {
		accessKeyID, err = a.awsPCARef(a.issuer.GetSpec().PrivateACM.AccessKeyIDRef.Name, a.issuer.GetSpec().PrivateACM.AccessKeyIDRef.Key)
		if err != nil {
			return nil, err
		}
	}

	if a.issuer.GetSpec().PrivateACM.SecretAccessKeyRef.Name != "" {
		secretKeyID, err = a.awsPCARef(a.issuer.GetSpec().PrivateACM.SecretAccessKeyRef.Name, a.issuer.GetSpec().PrivateACM.SecretAccessKeyRef.Key)
		if err != nil {
			return nil, err
		}
	}

	// Get the default cred chain
	awsDefaults := defaults.Get()
	defaultCredProviders := defaults.CredProviders(awsDefaults.Config, awsDefaults.Handlers)

	// Define custom static cred provider
	staticCreds := &credentials.StaticProvider{Value: credentials.Value{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretKeyID,
	}}

	// Append static creds to the defaults
	customCredProviders := append([]credentials.Provider{staticCreds}, defaultCredProviders...)
	creds := credentials.NewChainCredentials(customCredProviders)

	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String(a.issuer.GetSpec().PrivateACM.Region),
		Credentials: creds,
	})
	if err != nil {
		return nil, err
	}

	// if no region was set try fetching it from metadata
	if *sess.Config.Region == "" {
		metaSession, err := session.NewSession()
		if err != nil {
			return nil, err
		}

		metaClient := ec2metadata.New(metaSession)
		if metaClient.Available() {
			if region, err := metaClient.Region(); err == nil {
				sess.Config.Region = aws.String(region)
			} else {
				return nil, err
			}
		}
	}

	return acmpca.New(sess), nil
}

func (a *PrivateACM) awsPCARef(name, key string) (string, error) {
	secret, err := a.secretsLister.Secrets(a.resourceNamespace).Get(name)
	if err != nil {
		return "", err
	}

	if key == "" {
		key = "token"
	}

	keyBytes, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("no data for %q in secret '%s/%s'", key, name, a.resourceNamespace)
	}

	token := string(keyBytes)
	token = strings.TrimSpace(token)

	return token, nil
}
