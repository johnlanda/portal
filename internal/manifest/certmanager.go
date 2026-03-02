package manifest

import (
	"net"
	"time"
)

// buildSelfSignedIssuer creates a cert-manager Issuer with selfSigned configuration.
func buildSelfSignedIssuer(namespace string) (Resource, error) {
	issuer := map[string]interface{}{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Issuer",
		"metadata": map[string]interface{}{
			"name":      "portal-selfsigned-issuer",
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"selfSigned": map[string]interface{}{},
		},
	}
	return marshalResource("cert-manager-selfsigned-issuer.yaml", issuer)
}

// buildCACertificate creates a cert-manager Certificate for the tunnel CA.
// The CA duration is 3x the leaf validity.
func buildCACertificate(tunnelName, namespace string, leafValidity time.Duration) (Resource, error) {
	caDuration := (leafValidity * 3).String()
	cert := map[string]interface{}{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]interface{}{
			"name":      "portal-tunnel-ca",
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"isCA":       true,
			"commonName": "portal-ca",
			"secretName": "portal-tunnel-ca",
			"duration":   caDuration,
			"privateKey": map[string]interface{}{
				"algorithm": "RSA",
				"size":      4096,
			},
			"issuerRef": map[string]interface{}{
				"name":  "portal-selfsigned-issuer",
				"kind":  "Issuer",
				"group": "cert-manager.io",
			},
		},
	}
	return marshalResource("cert-manager-ca-certificate.yaml", cert)
}

// buildCAIssuer creates a cert-manager Issuer backed by the portal-tunnel-ca secret.
func buildCAIssuer(namespace string) (Resource, error) {
	issuer := map[string]interface{}{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Issuer",
		"metadata": map[string]interface{}{
			"name":      "portal-ca-issuer",
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"ca": map[string]interface{}{
				"secretName": "portal-tunnel-ca",
			},
		},
	}
	return marshalResource("cert-manager-ca-issuer.yaml", issuer)
}

// buildInitiatorCertificate creates a cert-manager Certificate for the initiator (client auth).
func buildInitiatorCertificate(tunnelName, namespace string, validity time.Duration) (Resource, error) {
	renewBefore := (validity / 3).String()
	cert := map[string]interface{}{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]interface{}{
			"name":      "portal-tunnel-tls",
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"commonName":  "portal-initiator/" + tunnelName,
			"secretName":  "portal-tunnel-tls",
			"duration":    validity.String(),
			"renewBefore": renewBefore,
			"usages": []interface{}{
				"client auth",
				"digital signature",
				"key encipherment",
			},
			"privateKey": map[string]interface{}{
				"algorithm":      "RSA",
				"size":           4096,
				"rotationPolicy": "Always",
			},
			"issuerRef": map[string]interface{}{
				"name":  "portal-ca-issuer",
				"kind":  "Issuer",
				"group": "cert-manager.io",
			},
		},
	}
	return marshalResource("cert-manager-initiator-certificate.yaml", cert)
}

// buildResponderCertificate creates a cert-manager Certificate for the responder (server auth).
func buildResponderCertificate(tunnelName, namespace string, validity time.Duration, sans []string) (Resource, error) {
	renewBefore := (validity / 3).String()

	dnsNames := []interface{}{}
	ipAddresses := []interface{}{}
	for _, san := range sans {
		if ip := net.ParseIP(san); ip != nil {
			ipAddresses = append(ipAddresses, san)
		} else {
			dnsNames = append(dnsNames, san)
		}
	}

	spec := map[string]interface{}{
		"commonName":  "portal-responder/" + tunnelName,
		"secretName":  "portal-tunnel-tls",
		"duration":    validity.String(),
		"renewBefore": renewBefore,
		"usages": []interface{}{
			"server auth",
			"digital signature",
			"key encipherment",
		},
		"privateKey": map[string]interface{}{
			"algorithm":      "RSA",
			"size":           4096,
			"rotationPolicy": "Always",
		},
		"issuerRef": map[string]interface{}{
			"name":  "portal-ca-issuer",
			"kind":  "Issuer",
			"group": "cert-manager.io",
		},
	}

	if len(dnsNames) > 0 {
		spec["dnsNames"] = dnsNames
	}
	if len(ipAddresses) > 0 {
		spec["ipAddresses"] = ipAddresses
	}

	cert := map[string]interface{}{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]interface{}{
			"name":      "portal-tunnel-tls",
			"namespace": namespace,
		},
		"spec": spec,
	}
	return marshalResource("cert-manager-responder-certificate.yaml", cert)
}

// buildCertManagerResources returns cert-manager CRDs split into source, destination,
// and shared (issuer chain) resource slices.
func buildCertManagerResources(tunnelName, namespace string, validity time.Duration, sans []string) (source, destination, shared []Resource, err error) {
	var sc resourceCollector
	sc.add(buildSelfSignedIssuer(namespace))
	sc.add(buildCACertificate(tunnelName, namespace, validity))
	sc.add(buildCAIssuer(namespace))
	if sc.err != nil {
		return nil, nil, nil, sc.err
	}

	var src resourceCollector
	src.add(buildInitiatorCertificate(tunnelName, namespace, validity))
	if src.err != nil {
		return nil, nil, nil, src.err
	}

	var dst resourceCollector
	dst.add(buildResponderCertificate(tunnelName, namespace, validity, sans))
	if dst.err != nil {
		return nil, nil, nil, dst.err
	}

	return src.resources, dst.resources, sc.resources, nil
}
