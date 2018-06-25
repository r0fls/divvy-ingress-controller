package controller

import (
	"bytes"
	"flag"
	"fmt"
	divvy "github.com/r0fls/divvy/pkg/loadbalancer"
	"io/ioutil"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"net"
	"net/http"
	"path/filepath"
	"sort"
)

var serviceName = "divvy-test-divvy-ingress-controller"

type Controller struct {
	Client         *kubernetes.Clientset
	Changes        chan Change
	PublishService bool
}

type Change struct {
	Type    watch.EventType
	Object  divvy.Worker
	Ingress *v1beta1.Ingress
}

const (
	// IngressKey picks a specific "class" for the Ingress.
	// The controller only processes Ingresses with this annotation either
	// unset, or set to either the configured value or the empty string.
	IngressKey = "kubernetes.io/ingress.class"
)

// Create the initial ingress resources
func (c *Controller) Create() {
	// Get the ingresses
	changes, _ := c.Client.ExtensionsV1beta1().Ingresses("").List(v1.ListOptions{})
	// Add ingresses with the apt class to syncstate channel
	for _, ingress := range changes.Items {
		// Only add this ingress to divvy if the ingress class is divvy
		class, ok := ingress.GetAnnotations()[IngressKey]
		if !ok || class != "divvy" {
			return
		}
		for _, rule := range ingress.Spec.Rules {
			//TODO: use path
			for _, path := range rule.IngressRuleValue.HTTP.Paths {
				c.Changes <- Change{
					Type: "ADDED",
					Object: divvy.Worker{
						Host:    rule.Host,
						Address: path.Backend.ServiceName,
						Port:    path.Backend.ServicePort.IntValue(),
					},
					Ingress: &ingress,
				}
			}
		}
	}
}

// Watch for changes and feed them to the changeset channel
func (c *Controller) Watch() {
	//TODO: configurable sync interval
	changes, _ := c.Client.ExtensionsV1beta1().Ingresses("").Watch(v1.ListOptions{})
	// Add ingresses with the apt class to syncstate channel
	for result := range changes.ResultChan() {
		// This will be ADDED/DELETED or maybe UPDATED
		ingress, _ := result.Object.(*v1beta1.Ingress)
		for _, rule := range ingress.Spec.Rules {
			for _, path := range rule.IngressRuleValue.HTTP.Paths {
				//TODO: use path
				c.Changes <- Change{
					Type: result.Type,
					Object: divvy.Worker{
						Host:    rule.Host,
						Address: path.Backend.ServiceName,
						Port:    path.Backend.ServicePort.IntValue(),
					},
					Ingress: ingress,
				}
			}
		}
	}
}

// sliceToStatus converts a slice of IP and/or hostnames to LoadBalancerIngress
func sliceToStatus(endpoints []string) []apiv1.LoadBalancerIngress {
	lbi := []apiv1.LoadBalancerIngress{}
	for _, ep := range endpoints {
		if net.ParseIP(ep) == nil {
			lbi = append(lbi, apiv1.LoadBalancerIngress{Hostname: ep})
		} else {
			lbi = append(lbi, apiv1.LoadBalancerIngress{IP: ep})
		}
	}

	sort.SliceStable(lbi, func(a, b int) bool {
		return lbi[a].IP < lbi[b].IP
	})

	return lbi
}

func (c *Controller) addBackend(change Change) {
	postData := fmt.Sprintf(`{"address":"%s", "port": %d, "host": "%s"}`, change.Object.Address, change.Object.Port, change.Object.Host)
	fmt.Printf(postData)
	var jsonStr = []byte(postData)
	req, err := http.NewRequest("POST", fmt.Sprintf("http://%s", serviceName), bytes.NewBuffer(jsonStr))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	fmt.Println(string(body))
	// Add the divvy service endpoint to the ingress loadbalancer status
	//TODO:
	// -- Do this somewhere else; watch the service LB, update it there, and update
	//    existing ingresses, and then store the result and use it here.
	if c.PublishService {

		svc, _ := c.Client.CoreV1().Services("").Get(serviceName, v1.GetOptions{})
		var addrs []string
		if svc.Spec.Type == apiv1.ServiceTypeExternalName {
			addrs = append(addrs, svc.Spec.ExternalName)
			change.Ingress.Status.LoadBalancer.Ingress = sliceToStatus(addrs)
			//TODO: update the ingress with k8s api
			_, err := c.Client.ExtensionsV1beta1().Ingresses("").UpdateStatus(change.Ingress)
			if err != nil {
				fmt.Println(err)
				return
			}
			return
		}
		for _, ip := range svc.Status.LoadBalancer.Ingress {
			if ip.IP == "" {
				addrs = append(addrs, ip.Hostname)
			} else {
				addrs = append(addrs, ip.IP)
			}
		}
		//TODO: update the ingress with k8s api
		change.Ingress.Status.LoadBalancer.Ingress = sliceToStatus(addrs)
		_, err := c.Client.ExtensionsV1beta1().Ingresses("").UpdateStatus(change.Ingress)
		if err != nil {
			fmt.Println(err)
			return
		}
	}
}

// Consume the changeset channel and update divvy/memory with the desired state
func (c *Controller) SyncState() {
	for change := range c.Changes {
		if change.Type == "ADDED" {
			c.addBackend(change)
		}
	}
}

func getInClusterClient() (*kubernetes.Clientset, error) {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	// creates the clientset
	return kubernetes.NewForConfig(config)
}

func getOutOfClusterClient() (*kubernetes.Clientset, error) {
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "minikube"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return clientset, nil
}

func getClusterClient() (*kubernetes.Clientset, error) {
	//TODO remove flag stuff and move to cobra/viper config
	//TODO in-cluser-config
	clientset, err := getInClusterClient()
	if err != nil {
		fmt.Println(err)
	} else {
		return clientset, err
	}
	return getOutOfClusterClient()

}

func NewController() *Controller {
	changes := make(chan Change)
	client, err := getClusterClient()
	if err != nil {
		panic(err)
	}
	return &Controller{Client: client, Changes: changes}
}

func (c *Controller) Start() {
	// This will write desired state to a channel
	go c.Create()
	go c.Watch()
	// This will consume desired state and compare it to known state,
	// then update the loadbalancer(s) and known state with the correct state.
	c.SyncState()
}
