package main

import (
	// "errors"
	"flag"
	"os/exec"
	"strconv"
	"strings"
	"bufio"
	"bytes"
	// "path"
	"syscall"
	"time"
	
	"sigs.k8s.io/sig-storage-lib-external-provisioner/controller"

	corev1 "k8s.io/api/core/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
)


func int32Ptr(i int32) *int32 { return &i }

func BytesToString(data []byte) string {
	return string(data[:])
}

type volumeNfsProvisioner struct {
}

// NewVolumeNfsProvisioner creates a new provisioner
func NewVolumeNfsProvisioner() controller.Provisioner {
	return &volumeNfsProvisioner{}
}

func RunExtCmd(name string, args ...string ) string {
	cmd := exec.Command(name, args...)
	stderr, err :=cmd.StderrPipe()
	if err != nil {
		klog.Info(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		klog.Info(err)
	}
	if err := cmd.Start(); err != nil {
		klog.Info(err)
	}
	sc := bufio.NewScanner(stderr)
	for sc.Scan() {
		klog.Info(sc.Text())
	}
	buf := new(bytes.Buffer)
	buf.ReadFrom(stdout)
	output := buf.String()
	return output
}

var _ controller.Provisioner = &volumeNfsProvisioner{}

// Provision creates a storage asset and returns a PV object representing it.
func (p *volumeNfsProvisioner) Provision(options controller.ProvisionOptions) (*corev1.PersistentVolume, error) {

	// nfsPvcNs := options.PVC.Namespace
	// nfsPvcName := options.PVC.Name
	nfsPvName	:= options.PVName
	nfsStsName	:= nfsPvName
	nfsSvcName	:= nfsStsName

	dataPvcNs := "volume-nfs"
	dataScName := options.StorageClass.Parameters[ "dataBackendStorageClass" ]

	nfsExporterImage := options.StorageClass.Parameters[ "nfsExporterImage" ]
	if nfsExporterImage == "" { nfsExporterImage = "daocloud.io/piraeus/volume-nfs-exporter:ganesha" }

	klog.Infof( "Data backend SC is \"%s\"", dataScName )
	klog.Infof( "NFS Exporter Image is \"%s\"", nfsExporterImage )

	dataPvcName := strings.Replace(nfsPvName, "pvc-", "data-", 1) + "-0"

	// create k8s clientset
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	klog.Info( "Created Kubernetes Client Set")

	// create Data backend PVC
	klog.Infof("Creating Data backend PVC \"%s\"", dataPvcName )
	capacity := options.PVC.Spec.Resources.Requests[corev1.ResourceName(corev1.ResourceStorage)]
	size := strconv.FormatInt( capacity.Value(), 10 )
	klog.Infof( "Data backend PVC size is \"%s\"", size )

	dataPvcDef := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: dataPvcName,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &dataScName,
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceName(corev1.ResourceStorage): resource.MustParse(size),
				},
			},
		},
	}

	dataPvc, err := clientSet.CoreV1().PersistentVolumeClaims(dataPvcNs).Create(dataPvcDef)
	if err != nil {
		panic(err)
	}

	dataPvcUid := ""
	for {
		dataPvcUid = string( dataPvc.ObjectMeta.UID );
		if dataPvcUid != "" {
			break
		}
	}

	klog.Infof("Data backend PVC uid is \"%s\"", dataPvcUid )

	dataPvName := "pvc-" + dataPvcUid

	// create NFS export SVC
	klog.Infof("NFS Export SVC \"%s\"", nfsSvcName )
	nfsSvcDef := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: nfsSvcName,
			Labels: map[string]string{
				"volume.io/nfs": nfsStsName,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
					"volume.io/nfs": nfsStsName,
				},
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name:          "nfs",
					Protocol:      corev1.ProtocolTCP,
					Port:   2049,
				},
				{
					Name:          "rpc",
					Protocol:      corev1.ProtocolTCP,
					Port:  111,
				},
			},
		},
	}

	_, err = clientSet.CoreV1().Services(dataPvcNs).Create(nfsSvcDef)
	if err != nil {
		panic(err)
	}

	nfsIp := ""
	for {
		time.Sleep(1 * time.Second)
		nfsSvc, err := clientSet.CoreV1().Services(dataPvcNs).Get(nfsSvcName, metav1.GetOptions{})
		if err == nil {
			nfsIp = nfsSvc.Spec.ClusterIP;
			klog.Infof("NFS export IP is \"%s\"", nfsIp )
		} else {
			klog.Infof( "Waiting for NFS SVC to spawn: \"%s\"", nfsSvcName )
		}
		if nfsIp != "" {
			break
		} 
	}



	// create NFS Export Pod to connect NFS Export SVC with Data backend PVC
	klog.Infof("Creating NFS export pod by StatefulSet: \"%s\"", nfsStsName )

	nfsStsDef := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: nfsStsName,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: int32Ptr(1),
			ServiceName: nfsStsName,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"volume.io/nfs": nfsStsName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"volume.io/nfs": nfsStsName,
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
									Name:          "rpc",
									Protocol:      corev1.ProtocolTCP,
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
									Name: "data",
									MountPath: "/" + dataPvName,
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

	_, err = clientSet.AppsV1().StatefulSets(dataPvcNs).Create(nfsStsDef)
	if err != nil {
		panic(err)
	}

	// wait until NFS Pod is ready
	nfsPodName := nfsStsName + "-0"
	nfsPodStatus := corev1.PodUnknown
	for {
		time.Sleep(1 * time.Second)
		nfsPod, err := clientSet.CoreV1().Pods(dataPvcNs).Get(nfsPodName, metav1.GetOptions{})
		if err == nil {
			nfsPodStatus = nfsPod.Status.Phase;
			klog.Infof( "NFS export Pod status is: \"%s\"", nfsPodStatus )
		} else {
			klog.Infof( "Waiting for NFS export Pod to spawn: \"%s\"", nfsPodName )
		}
		if nfsPodStatus == corev1.PodRunning {
			break
		}
	}

	// Create NFS PV (and return it)
	nfsPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: options.PVName,
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: *options.StorageClass.ReclaimPolicy,
			AccessModes:                   options.PVC.Spec.AccessModes,
			Capacity: corev1.ResourceList{
				corev1.ResourceName(corev1.ResourceStorage): options.PVC.Spec.Resources.Requests[corev1.ResourceName(corev1.ResourceStorage)],
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
	return nfsPV, nil
}

// Delete removes the storage asset that was created by Provision represented
// by the given PV.
func (p *volumeNfsProvisioner) Delete(volume *corev1.PersistentVolume) error {
	nfsPvName := volume.ObjectMeta.Name
	nfsStsName := nfsPvName
	nfsSvcName := nfsStsName
	dataPvcName := strings.Replace(nfsStsName, "pvc-", "data-", 1) + "-0"
	dataPvcNs := "volume-nfs"

	// create k8s clientset
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	klog.Info( "Created Kubernetes Client Set")

	// delete NFS export pod, NFS export svc, and Data Backend PVC
	klog.Infof("Deleting NFS export Pod by StatfulSet: \"%s\"", nfsStsName )
	err = clientSet.AppsV1().StatefulSets(dataPvcNs).Delete(nfsStsName, &metav1.DeleteOptions{})
	klog.Infof("Deleting NFS export SVC: \"%s\"", nfsSvcName )
	err = clientSet.CoreV1().Services(dataPvcNs).Delete(nfsSvcName, &metav1.DeleteOptions{})
	klog.Infof("Deleting Data backend PVC: \"%s\"", dataPvcName )
	err = clientSet.CoreV1().PersistentVolumeClaims(dataPvcNs).Delete(dataPvcName, &metav1.DeleteOptions{})
	return nil
}

func main() {
	syscall.Umask(0)

	// Provisoner name
	provisionerName := flag.String("name", "nfs.volume.io", "a string")

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
	volumeNfsProvisioner := NewVolumeNfsProvisioner()

	// Start the provision controller
	// PVs
	pc := controller.NewProvisionController(clientset, *provisionerName, volumeNfsProvisioner, serverVersion.GitVersion)
	pc.Run(wait.NeverStop)
}