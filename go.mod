module github.com/maddinenisri/cf-k8s-operator

go 1.15

require (
	github.com/aws/aws-sdk-go v1.36.29
	github.com/go-logr/logr v0.3.0
	github.com/onsi/ginkgo v1.14.1
	github.com/onsi/gomega v1.10.2
	github.com/sirupsen/logrus v1.7.0
	github.com/spf13/pflag v1.0.5
	golang.org/x/sys v0.0.0-20210119212857-b64e53b001e4 // indirect
	k8s.io/api v0.19.2
	k8s.io/apimachinery v0.19.2
	k8s.io/client-go v0.19.2
	sigs.k8s.io/controller-runtime v0.7.0
)
