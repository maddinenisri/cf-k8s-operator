
# Cloudformation K8s Operator

```sh
  operator-sdk init --domain=mdstechinc.com --repo=github.com/maddinenisri/cf-k8s-operator

  operator-sdk create api --group=cloudformation --version=v1alpha1 --kind=Stack

```

- Generate manifests after updating API
```sh
  make manifests
```

- After Controller changes
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

- Prebuilt image "mdstech/cf-k8s-operator:latest" can be used to avoid build steps

- Edit "config/default/kustomization.yaml" namespace to "default"
- Perform deploy
```sh
  make deploy
```
