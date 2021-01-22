
# Cloudformation K8s Operator

- New project creation commands
```sh
  operator-sdk init --domain=mdstechinc.com --repo=github.com/maddinenisri/cf-k8s-operator
  operator-sdk create api --group=cloudformation --version=v1alpha1 --kind=Stack
```
- Modify API spec and specify kubebuilder annotations in API struct
- Generate manifests after updating API
```sh
  make manifests
```
- Implement controller business logic
  
- After Controller changes build project
```sh
  make install
```

- Perform docker build
```sh
  make docker-build
```

- Perform docker push
```sh
  make docker-push
```

- Prebuild docker image
```sh
  docker pull mdstech/cf-k8s-operator:latest
```

- Edit "config/default/kustomization.yaml" namespace to "default"
- Perform deploy
```sh
  make deploy
```
# Reference: 
[cloudformation-operator](https://github.com/linki/cloudformation-operator)
