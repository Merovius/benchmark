The private/public keys cert.pem and key.pem are used for loadtests only, not
in production. To ensure this, their Subject Alternative Name is set to
“localhost”, “robustirc-node-1”, “robustirc-node-2” and “robustirc-node-3”,
none of which are hostnames that resolve to public addresses.
