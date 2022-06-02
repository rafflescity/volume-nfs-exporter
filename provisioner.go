package main

import (
	"flag"
	"strconv"
	"syscall"
	"time"
	"context"
	
	"sigs.k8s.io/sig-storage-lib-external-provisioner/v6/controller"

	corev1 "k8s.io/api/core/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
)


func int32Ptr(i int32) *int32 { return &i }

type volumeNfsProvisioner struct {
	clientSet *kubernetes.Clientset
}

// NewVolumeNfsProvisioner creates a new provisioner
func NewVolumeNfsProvisioner(cs *kubernetes.Clientset) controller.Provisioner {
	return &volumeNfsProvisioner{
		clientSet: cs,
	}
}

var _ controller.Provisioner = &volumeNfsProvisioner{}

// Provision creates a storage asset and returns a PV object representing it.
func (p *volumeNfsProvisioner) Provision(ctx context.Context, options controller.ProvisionOptions) (*corev1.PersistentVolume, controller.ProvisioningState, error) {

	nfsPvcNs := options.PVC.Namespace
	nfsPvcName := options.PVC.Name
	nfsPvcTitle := "[" + nfsPvcNs + "/" + nfsPvcName + "] "
	nfsPvName	:= options.PVName
	nfsStsName	:= nfsPvName + "-nfs-backend"
	nfsSvcName	:= nfsStsName

	dataPvcNs := "volume-nfs"
	dataScName := options.StorageClass.Parameters[ "dataBackendStorageClass" ]

	nfsExporterImage := options.StorageClass.Parameters[ "nfsExporterImage" ]
	if nfsExporterImage == "" { nfsExporterImage = "daocloud.io/piraeus/volume-nfs-exporter:ganesha" }

	klog.Infof( nfsPvcTitle + "data backend SC is \"%s\"", dataScName )
	klog.Infof( nfsPvcTitle + "NFS Exporter Image is \"%s\"", nfsExporterImage )

	dataPvcName := nfsStsName + "-0"

	// create data backend PVC
	klog.Infof(nfsPvcTitle + "Creating data backend PVC \"%s\"", dataPvcName )
	capacity := options.PVC.Spec.Resources.Requests[corev1.ResourceName(corev1.ResourceStorage)]
	size := strconv.FormatInt( capacity.Value(), 10 )
	klog.Infof( nfsPvcTitle + "data backend PVC size is \"%s\"", size )

	dataPvcDef := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: dataPvcName,
			Labels: map[string]string{
				"nfs.volume.io/data-sts": nfsStsName,
				"nfs.volume.io/nfs-pvc": nfsPvcName,
				"nfs.volume.io/nfs-pvc-namespace": nfsPvcNs,
				"nfs.volume.io/nfs-pv": nfsPvName,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &dataScName,
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceName(corev1.ResourceStorage): capacity,
				},
			},
		},
	}

	dataPvc, err := p.clientSet.CoreV1().PersistentVolumeClaims(dataPvcNs).Create(context.TODO(),dataPvcDef, metav1.CreateOptions{})
	if err != nil {
		panic(err)
	}

	dataPvcUid := dataPvc.ObjectMeta.UID
	dataPvcUidStr := string(dataPvcUid)

	klog.Infof(nfsPvcTitle + "data backend PVC uid is \"%s\"", dataPvcUidStr )

	dataPvName := "pvc-" + dataPvcUidStr

	// create NFS export SVC
	klog.Infof(nfsPvcTitle + "NFS export SVC \"%s\"", nfsSvcName )
	nfsSvcDef := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: nfsSvcName,
			Labels: map[string]string{
				"nfs.volume.io/nfs-pvc": nfsPvcName,
				"nfs.volume.io/nfs-pvc-namespace": nfsPvcNs,
				"nfs.volume.io/nfs-pv": nfsPvName,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "PersistentVolumeClaim",
					Name:               dataPvcName,
					UID:                dataPvcUid,
				},
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
					"nfs.volume.io/nfs-pv": nfsPvName,
				},
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:		"nfs",
					Protocol:	corev1.ProtocolTCP,
					Port:		2049,
				},
				{
					Name:		"rpc-tcp",
					Protocol:	corev1.ProtocolTCP,
					Port:		111,
				},
				{
					Name:		"rpc-udp",
					Protocol:	corev1.ProtocolUDP,
					Port:		111,
				},
			},
		},
	}

	_, err = p.clientSet.CoreV1().Services(dataPvcNs).Create(context.TODO(), nfsSvcDef, metav1.CreateOptions{})
	if err != nil {
		panic(err)
	}

	nfsIp := ""
	for {
		time.Sleep(1 * time.Second)
		nfsSvc, err := p.clientSet.CoreV1().Services(dataPvcNs).Get(context.TODO(), nfsSvcName, metav1.GetOptions{})
		if err == nil {
			nfsIp = nfsSvc.Spec.ClusterIP;
			klog.Infof(nfsPvcTitle + "NFS export IP is \"%s\"", nfsIp)
		} else {
			klog.Infof(nfsPvcTitle + "Waiting for NFS SVC to spawn: \"%s\"", nfsSvcName)
		}
		if nfsIp != "" {
			break
		} 
	}

	// create NFS export Pod to connect NFS export SVC with data backend PVC
	klog.Infof(nfsPvcTitle + "Creating NFS export pod by StatefulSet: \"%s\"", nfsStsName)

	nfsStsDef := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: nfsStsName,
			Labels: map[string]string{
				"nfs.volume.io/nfs-pvc": nfsPvcName,
				"nfs.volume.io/nfs-pvc-namespace": nfsPvcNs,
				"nfs.volume.io/nfs-pv": nfsPvName,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "PersistentVolumeClaim",
					Name:               dataPvcName,
					UID:                dataPvcUid,
				},
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: int32Ptr(1),
			ServiceName: nfsStsName,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"nfs.volume.io/nfs-pv": nfsPvName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"nfs.volume.io/nfs-pvc": nfsPvcName,
						"nfs.volume.io/nfs-pvc-namespace": nfsPvcNs,
						"nfs.volume.io/nfs-pv": nfsPvName,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "export",
							Image: nfsExporterImage,
							ImagePullPolicy: corev1.PullAlways,
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{
										"SYS_ADMIN",
										"SETPCAP",
										"DAC_READ_SEARCH",
									},
								},
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "nfs",
									Protocol:      corev1.ProtocolTCP,
									ContainerPort: 2049,
								},
								{
									Name:          "rpc-tcp",
									Protocol:      corev1.ProtocolTCP,
									ContainerPort: 111,
								},
								{
									Name:          "rpc-udp",
									Protocol:      corev1.ProtocolUDP,
									ContainerPort: 111,
								},
							},
							Env: []corev1.EnvVar{
								{
									Name: "EXPORT_PATH",
									Value: "/" + dataPvName,
								},
								{
									Name: "PSEUDO_PATH",
									Value: "/" + dataPvName,
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:		"data",
									MountPath:	"/" + dataPvName,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: dataPvcName,
								},
							},
						},
					},
				},
			},
		},
	}

	_, err = p.clientSet.AppsV1().StatefulSets(dataPvcNs).Create(context.TODO(), nfsStsDef, metav1.CreateOptions{})
	if err != nil {
		panic(err)
	}

	// wait until NFS Pod is ready
	nfsPodName := nfsStsName + "-0"
	nfsPodStatus := corev1.PodUnknown
	for {
		time.Sleep(1 * time.Second)
		nfsPod, err := p.clientSet.CoreV1().Pods(dataPvcNs).Get(context.TODO(), nfsPodName, metav1.GetOptions{})
		if err == nil {
			nfsPodStatus = nfsPod.Status.Phase;
			klog.Infof( nfsPvcTitle + "NFS export Pod status is: \"%s\"", nfsPodStatus )
		} else {
			klog.Infof( nfsPvcTitle + "Waiting for NFS export Pod to spawn: \"%s\"", nfsPodName )
		}
		if nfsPodStatus == corev1.PodRunning {
			break
		}
	}

	// create NFS PV (and return it)
	nfsPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: options.PVName,
			Labels: map[string]string{
				"nfs.volume.io/data-pvc": dataPvcName,
				"nfs.volume.io/data-pvc-namespace": dataPvcName,
				"nfs.volume.io/data-pv": dataPvName,
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: *options.StorageClass.ReclaimPolicy,
			AccessModes:                   options.PVC.Spec.AccessModes,
			Capacity: corev1.ResourceList{
				corev1.ResourceName(corev1.ResourceStorage): capacity,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{
					Server:   nfsIp,
					Path:     "/" + dataPvName,
					ReadOnly: false,
				},
			},
		},
	}
	return nfsPV, controller.ProvisioningFinished, nil
}

// Delete removes the storage asset that was created by Provision represented
// by the given PV.
func (p *volumeNfsProvisioner) Delete(ctx context.Context, volume *corev1.PersistentVolume) error {
	nfsPvName := volume.ObjectMeta.Name
	nfsStsName	:= nfsPvName + "-nfs-backend"
	nfsSvcName := nfsStsName
	dataPvcName := nfsStsName + "-0"
	dataPvcNs := "volume-nfs"

	deletePolicy := metav1.DeletePropagationForeground
	deleteOptions := metav1.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	}

	nfsPvcNs := volume.Spec.ClaimRef.Namespace
	nfsPvcName := volume.Spec.ClaimRef.Name
	nfsPvcTitle := "[" + nfsPvcNs + "/" + nfsPvcName + "] "

	// delete NFS export pod, NFS export svc, and data backend PVC
	// klog.Infof("Deleting NFS export Pod by StatfulSet: \"%s\"", nfsStsName )
	// _ = p.clientSet.AppsV1().StatefulSets(dataPvcNs).Delete(context.TODO(), nfsStsName, deleteOptions)
	// klog.Infof("Deleting NFS export SVC: \"%s\"", nfsSvcName )
	// _ = p.clientSet.CoreV1().Services(dataPvcNs).Delete(context.TODO(), nfsSvcName, deleteOptions)
	klog.Infof( nfsPvcTitle + "Deleting data backend PVC: \"%s\"", dataPvcName )
	_ = p.clientSet.CoreV1().PersistentVolumeClaims(dataPvcNs).Delete(context.TODO(), dataPvcName, deleteOptions)
	klog.Infof(nfsPvcTitle + "Deleting (by ownerReference) NFS export StatfulSet: \"%s\"", nfsStsName )
	klog.Infof(nfsPvcTitle + "Deleting (by ownerReference) NFS export SVC: \"%s\"", nfsSvcName )
	return nil
}

func main() {
	syscall.Umask(0)

	// Cmd Options
	provisionerName := flag.String("name", "nfs.volume.io", "Set the provisoner name. Default \"nfs.volume.io\"")
	leaderElection := flag.Bool("leader-elect", false, "Start a leader election client and gain leadership before executing the main loop. Enable this when running replicated components for high availability. Default false.")

	
	flag.Parse()
	flag.Set("logtostderr", "true")

	// Create an InClusterConfig and use it to create a client for the controller
	// to use to communicate with Kubernetes
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Failed to create config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create client: %v", err)
	}

	// The controller needs to know what the server version is because out-of-tree
	// provisioners aren't officially supported until 1.5
	serverVersion, err := clientset.Discovery().ServerVersion()
	if err != nil {
		klog.Fatalf("Error getting server version: %v", err)
	}

	// Create the provisioner: it implements the Provisioner interface expected by
	// the controller
	volumeNfsProvisioner := NewVolumeNfsProvisioner(clientset)

	// Start the provision controller
	// PVs
	pc := controller.NewProvisionController(
		clientset, 
		*provisionerName, 
		volumeNfsProvisioner, 
		serverVersion.GitVersion,
		controller.LeaderElection(*leaderElection),
	)
	
	// Never stops.
	pc.Run(context.Background())
}