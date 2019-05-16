=====================================
Setting up AWS ACM Private CA Issuers
=====================================

AWS ACM Private CA Issuer allows you to obraine certificate from AWS ACM_

Create `Private CA`_ and get its ARN

Creating an Issuer resource
===========================

A single issuer can work with single AWS region, so you need to create and issuer
per region you want to work with.

Authentication
--------------

ACM issuer need credentials to authenticase with AWS. Create secret containing
AWS key id and AWS secret:

.. code-block:: shell

   kubectl create secret generic \
        aws-secret \
        --namespace='NAMESPACE OF YOUR ISSUER RESOURCE' \
        --from-literal=key='AWS KEY ID HERE' \
        --form-literal=secret='AWS SECRET HERE'

Once the secret has been created, you can create your Issuer or
ClusterIssuer resource. If you are creating a ClusterIssuer resource, you must
change the ``kind`` field to ``ClusterIssuer`` and remove the
``metadata.namespace`` field.

Apply below configuration to create ``Issuer``:

.. code-block:: yaml

   apiVersion: certmanager.k8s.io/v1alpha1
   kind: Issuer
   metadata:
     name: acm-issuer
     namespace: <NAMESPACE YOU WANT TO ISSUE CERTIFICATES IN>
   spec:
     privateACM:
       certificateAuthorityARN: arn:aws:acm-pca:us-east-1:123456789:certificate-authority/738qifs-123oq-3ed4-32we-143edfw3rcxqd
       accessKeyIdRef:
         ame: aws-secret # secret name created before
         key: key
       secretAccessKeyRef:
         name: aws-secret
         key: secret
       region: us-east-1 # specify region where Private CA was created

You are now ready to issue certificates using the newly provisioned Private ACM
Issuer.


.. _ACM: https://aws.amazon.com/certificate-manager/private-certificate-authority/
.. _Private CA: https://console.aws.amazon.com/acm-pca/home#/certificateAuthorities
