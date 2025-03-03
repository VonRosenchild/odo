package util

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gobwas/glob"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyz")

// 63 is the max length of a DeploymentConfig in Openshift and we also have to take into account
// that each component also gets a volume that uses the component name suffixed with -s2idata
const maxAllowedNamespacedStringLength = 63 - len("-s2idata") - 1

// This value can be provided to set a seperate directory for users 'homedir' resolution
// note for mocking purpose ONLY
var customHomeDir = os.Getenv("CUSTOM_HOMEDIR")

// ResourceRequirementInfo holds resource quantity before transformation into its appropriate form in container spec
type ResourceRequirementInfo struct {
	ResourceType corev1.ResourceName
	MinQty       resource.Quantity
	MaxQty       resource.Quantity
}

// ConvertLabelsToSelector converts the given labels to selector
func ConvertLabelsToSelector(labels map[string]string) string {
	var selector string
	isFirst := true
	for k, v := range labels {
		if isFirst {
			isFirst = false
			if v == "" {
				selector = selector + fmt.Sprintf("%v", k)
			} else {
				selector = fmt.Sprintf("%v=%v", k, v)
			}
		} else {
			if v == "" {
				selector = selector + fmt.Sprintf(",%v", k)
			} else {
				selector = selector + fmt.Sprintf(",%v=%v", k, v)
			}
		}
	}
	return selector
}

// GenerateRandomString generates a random string of lower case characters of
// the given size
func GenerateRandomString(n int) string {
	rand.Seed(time.Now().UnixNano())
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

// In checks if the value is in the array
func In(arr []string, value string) bool {
	for _, item := range arr {
		if item == value {
			return true
		}
	}
	return false
}

// Hyphenate applicationName and componentName
func NamespaceOpenShiftObject(componentName string, applicationName string) (string, error) {

	// Error if it's blank
	if componentName == "" {
		return "", errors.New("namespacing: component name cannot be blank")
	}

	// Error if it's blank
	if applicationName == "" {
		return "", errors.New("namespacing: application name cannot be blank")
	}

	// Return the hyphenated namespaced name

	originalName := fmt.Sprintf("%s-%s", strings.Replace(componentName, "/", "-", -1), applicationName)
	truncatedName := TruncateString(originalName, maxAllowedNamespacedStringLength)
	if originalName != truncatedName {
		glog.V(4).Infof("The combination of application %s and component %s was too long so the final name was truncated to %s",
			applicationName, componentName, truncatedName)
	}
	return truncatedName, nil
}

// ExtractComponentType returns only component type part from passed component type(default unqualified, fully qualified, versioned, etc...and their combinations) for use as component name
// Possible types of parameters:
// 1. "myproject/python:3.5" -- Return python
// 2. "python:3.5" -- Return python
// 3. nodejs -- Return nodejs
func ExtractComponentType(namespacedVersionedComponentType string) string {
	s := strings.Split(namespacedVersionedComponentType, "/")
	versionedString := s[0]
	if len(s) == 2 {
		versionedString = s[1]
	}
	s = strings.Split(versionedString, ":")
	return s[0]
}

// ParseComponentImageName returns
// 1. image name
// 2. component type i.e, builder image name
// 3. component name default value is component type else the user requested component name
// 4. component version which is by default latest else version passed with builder image name
func ParseComponentImageName(imageName string) (string, string, string, string) {
	// We don't have to check it anymore, Args check made sure that args has at least one item
	// and no more than two

	// "Default" values
	componentImageName := imageName
	componentType := imageName
	componentName := ExtractComponentType(componentType)
	componentVersion := "latest"

	// Check if componentType includes ":", if so, then we need to spit it into using versions
	if strings.ContainsAny(componentImageName, ":") {
		versionSplit := strings.Split(imageName, ":")
		componentType = versionSplit[0]
		componentName = ExtractComponentType(componentType)
		componentVersion = versionSplit[1]
	}
	return componentImageName, componentType, componentName, componentVersion
}

const WIN = "windows"

// ReadFilePath Reads file path form URL file:///C:/path/to/file to C:\path\to\file
func ReadFilePath(u *url.URL, os string) string {
	location := u.Path
	if os == WIN {
		location = strings.Replace(u.Path, "/", "\\", -1)
		location = location[1:]
	}
	return location
}

// GenFileURL Converts file path on windows to /C:/path/to/file to work in URL
func GenFileURL(location string, os ...string) string {
	// param os is made variadic only for the purpose of UTs but need not be passed mandatorily
	currOS := runtime.GOOS
	if len(os) > 0 {
		currOS = os[0]
	}
	urlPath := location
	if currOS == WIN {
		urlPath = "/" + strings.Replace(location, "\\", "/", -1)
	}
	return "file://" + urlPath
}

// ConvertKeyValueStringToMap converts String Slice of Parameters to a Map[String]string
// Each value of the slice is expected to be in the key=value format
// Values that do not conform to this "spec", will be ignored
func ConvertKeyValueStringToMap(params []string) map[string]string {
	result := make(map[string]string, len(params))
	for _, param := range params {
		str := strings.Split(param, "=")
		if len(str) != 2 {
			glog.Fatalf("Parameter %s is not in the expected key=value format", param)
		} else {
			result[str[0]] = str[1]
		}
	}
	return result
}

// TruncateString truncates passed string to given length
// Note: if -1 is passed, the original string is returned
func TruncateString(str string, maxLen int) string {
	if maxLen == -1 {
		return str
	}
	if len(str) > maxLen {
		return str[:maxLen]
	}
	return str
}

// GetAbsPath returns absolute path from passed file path resolving even ~ to user home dir and any other such symbols that are only
// shell expanded can also be handled here
func GetAbsPath(path string) (string, error) {
	// Only shell resolves `~` to home so handle it specially
	var dir string
	if strings.HasPrefix(path, "~") {
		if len(customHomeDir) > 0 {
			dir = customHomeDir
		} else {
			usr, err := user.Current()
			if err != nil {
				return path, errors.Wrapf(err, "unable to resolve %s to absolute path", path)
			}
			dir = usr.HomeDir
		}

		if len(path) > 1 {
			path = filepath.Join(dir, path[1:])
		} else {
			path = dir
		}
	}

	path, err := filepath.Abs(path)
	if err != nil {
		return path, errors.Wrapf(err, "unable to resolve %s to absolute path", path)
	}
	return path, nil
}

// GetRandomName returns a randomly generated name which can be used for naming odo and/or openshift entities
// prefix: Desired prefix part of the name
// prefixMaxLen: Desired maximum length of prefix part of random name; if -1 is passed, no limit on length will be enforced
// existList: List to verify that the returned name does not already exist
// retries: number of retries to try generating a unique name
// Returns:
//		1. randomname: is prefix-suffix, where:
//				prefix: string passed as prefix or fetched current directory of length same as the passed prefixMaxLen
//				suffix: 4 char random string
//      2. error: if requested number of retries also failed to generate unique name
func GetRandomName(prefix string, prefixMaxLen int, existList []string, retries int) (string, error) {
	prefix = TruncateString(GetDNS1123Name(strings.ToLower(prefix)), prefixMaxLen)
	name := fmt.Sprintf("%s-%s", prefix, GenerateRandomString(4))

	//Create a map of existing names for efficient iteration to find if the newly generated name is same as any of the already existing ones
	existingNames := make(map[string]bool)
	for _, existingName := range existList {
		existingNames[existingName] = true
	}

	// check if generated name is already used in the existList
	if _, ok := existingNames[name]; ok {
		prevName := name
		trial := 0
		// keep generating names until generated name is not unique. So, loop terminates when name is unique and hence for condition is false
		for ok {
			trial = trial + 1
			prevName = name
			// Attempt unique name generation from prefix-suffix by concatenating prefix-suffix withrandom string of length 4
			prevName = fmt.Sprintf("%s-%s", prevName, GenerateRandomString(4))
			_, ok = existingNames[prevName]
			if trial >= retries {
				// Avoid infinite loops and fail after passed number of retries
				return "", fmt.Errorf("failed to generate a unique name even after %d retrials", retries)
			}
		}
		// If found to be unique, set name as generated name
		name = prevName
	}
	// return name
	return name, nil
}

// GetDNS1123Name Converts passed string into DNS-1123 string
func GetDNS1123Name(str string) string {
	nonAllowedCharsRegex := regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
	withReplacedChars := strings.Replace(
		nonAllowedCharsRegex.ReplaceAllString(str, "-"),
		"--", "-", -1)
	return removeNonAlphaSuffix(removeNonAlphaPrefix(withReplacedChars))
}

func removeNonAlphaPrefix(input string) string {
	regex := regexp.MustCompile("^[^a-zA-Z0-9]+(.*)$")
	return regex.ReplaceAllString(input, "$1")
}

func removeNonAlphaSuffix(input string) string {
	suffixRegex := regexp.MustCompile("^(.*?)[^a-zA-Z0-9]+$") //regex that strips all trailing non alpha-numeric chars
	matches := suffixRegex.FindStringSubmatch(input)
	matchesLength := len(matches)
	if matchesLength == 0 {
		// in this case the string does not contain a non-alphanumeric suffix
		return input
	} else {
		// in this case we return the smallest match which in the last element in the array
		return matches[matchesLength-1]
	}
}

// SliceDifference returns the values of s2 that do not exist in s1
func SliceDifference(s1 []string, s2 []string) []string {
	mb := map[string]bool{}
	for _, x := range s1 {
		mb[x] = true
	}
	difference := []string{}
	for _, x := range s2 {
		if _, ok := mb[x]; !ok {
			difference = append(difference, x)
		}
	}
	return difference
}

// OpenBrowser opens the URL within the users default browser
func OpenBrowser(url string) error {
	var err error

	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("unsupported platform")
	}
	if err != nil {
		return err
	}

	return nil
}

// FetchResourceQuantity takes passed min, max and requested resource quantities and returns min and max resource requests
func FetchResourceQuantity(resourceType corev1.ResourceName, min string, max string, request string) (*ResourceRequirementInfo, error) {
	if min == "" && max == "" && request == "" {
		return nil, nil
	}
	// If minimum and maximum both are passed they carry highest priority
	// Otherwise, use the request as min and max
	var minResource resource.Quantity
	var maxResource resource.Quantity
	if min != "" {
		resourceVal, err := resource.ParseQuantity(min)
		if err != nil {
			return nil, err
		}
		minResource = resourceVal
	}
	if max != "" {
		resourceVal, err := resource.ParseQuantity(max)
		if err != nil {
			return nil, err
		}
		maxResource = resourceVal
	}
	if request != "" && (min == "" || max == "") {
		resourceVal, err := resource.ParseQuantity(request)
		if err != nil {
			return nil, err
		}
		minResource = resourceVal
		maxResource = resourceVal
	}
	return &ResourceRequirementInfo{
		ResourceType: resourceType,
		MinQty:       minResource,
		MaxQty:       maxResource,
	}, nil
}

// CheckPathExists checks if a path exists or not
func CheckPathExists(path string) bool {
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		// path to file does exist
		return true
	}
	glog.V(4).Infof("path %s doesn't exist, skipping it", path)
	return false
}

// GetHostWithPort parses provided url and returns string formated as
// host:port even if port was not specifically specified in the origin url.
// If port is not specified, standart port corresponding to url schema is provided.
// example: for url https://example.com function will return "example.com:443"
//          for url https://example.com:8443 function will return "example:8443"
func GetHostWithPort(inputURL string) (string, error) {
	u, err := url.Parse(inputURL)
	if err != nil {
		return "", errors.Wrapf(err, "error while getting port for url %s ", inputURL)
	}

	port := u.Port()
	address := u.Host
	// if port is not specified try to detect it based on provided scheme
	if port == "" {
		portInt, err := net.LookupPort("tcp", u.Scheme)
		if err != nil {
			return "", errors.Wrapf(err, "error while getting port for url %s ", inputURL)
		}
		port = strconv.Itoa(portInt)
		address = fmt.Sprintf("%s:%s", u.Host, port)
	}
	return address, nil
}

// GetIgnoreRulesFromDirectory reads the .odoignore file, if present, and reads the rules from it
// if the .odoignore file is not found, then .gitignore is searched for the rules
// if both are not found, return empty array
// directory is the name of the directory to look into for either of the files
// rules is the array of rules (in string form)
func GetIgnoreRulesFromDirectory(directory string) ([]string, error) {
	rules := []string{".git"}
	// checking for presence of .odoignore file
	pathIgnore := filepath.Join(directory, ".odoignore")
	if _, err := os.Stat(pathIgnore); os.IsNotExist(err) || err != nil {
		// .odoignore doesn't exist
		// checking presence of .gitignore file
		pathIgnore = filepath.Join(directory, ".gitignore")
		if _, err := os.Stat(pathIgnore); os.IsNotExist(err) || err != nil {
			// both doesn't exist, return empty array
			return rules, nil
		}
	}

	file, err := os.Open(pathIgnore)
	if err != nil {
		return nil, err
	}

	defer file.Close()

	scanner := bufio.NewReader(file)
	for {
		line, _, err := scanner.ReadLine()
		if err != nil {
			if err == io.EOF {
				break
			}

			return rules, err
		}
		spaceTrimmedLine := strings.TrimSpace(string(line))
		if len(spaceTrimmedLine) > 0 && !strings.HasPrefix(string(line), "#") && !strings.HasPrefix(string(line), ".git") {
			rules = append(rules, string(line))
		}
	}

	return rules, nil
}

// GetAbsGlobExps converts the relative glob expressions into absolute glob expressions
// returns the absolute glob expressions
func GetAbsGlobExps(directory string, globExps []string) []string {
	absGlobExps := []string{}
	for _, globExp := range globExps {
		// for glob matching with the library
		// the relative paths in the glob expressions need to be converted to absolute paths
		absGlobExps = append(absGlobExps, filepath.Join(directory, globExp))
	}
	return absGlobExps
}

// GetSortedKeys retrieves the alphabetically-sorted keys of the specified map
func GetSortedKeys(mapping map[string]string) []string {
	keys := make([]string, len(mapping))

	i := 0
	for k := range mapping {
		keys[i] = k
		i++
	}

	sort.Strings(keys)

	return keys
}

//returns a slice containing the split string, using ',' as a separator
func GetSplitValuesFromStr(inputStr string) []string {
	if len(inputStr) == 0 {
		return []string{}
	}

	result := strings.Split(inputStr, ",")
	for i, value := range result {
		result[i] = strings.TrimSpace(value)
	}
	return result
}

// GetContainerPortsFromStrings generates ContainerPort values from the array of string port values
// ports is the array containing the string port values
func GetContainerPortsFromStrings(ports []string) ([]corev1.ContainerPort, error) {
	var containerPorts []corev1.ContainerPort
	for _, port := range ports {
		splits := strings.Split(port, "/")
		if len(splits) < 1 || len(splits) > 2 {
			return nil, fmt.Errorf("unable to parse the port string %s", port)
		}

		portNumberI64, err := strconv.ParseInt(splits[0], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid port number %s", splits[0])
		}
		portNumber := int32(portNumberI64)

		var portProto corev1.Protocol
		if len(splits) == 2 {
			switch strings.ToUpper(splits[1]) {
			case "TCP":
				portProto = corev1.ProtocolTCP
			case "UDP":
				portProto = corev1.ProtocolUDP
			default:
				return nil, fmt.Errorf("invalid port protocol %s", splits[1])
			}
		} else {
			portProto = corev1.ProtocolTCP
		}

		port := corev1.ContainerPort{
			Name:          fmt.Sprintf("%d-%s", portNumber, strings.ToLower(string(portProto))),
			ContainerPort: portNumber,
			Protocol:      portProto,
		}
		containerPorts = append(containerPorts, port)
	}
	return containerPorts, nil
}

// IsGlobExpMatch compiles strToMatch against each of the passed globExps
// Parameters:
// strToMatch : a string for matching against the rules
// globExps : a list of glob patterns to match strToMatch with
// Returns: true if there is any match else false the error (if any)
// Notes:
// Source as well as glob expression to match is changed to forward
// slashes due to supporting Windows as well as support with the
// "github.com/gobwas/glob" library that we use.
func IsGlobExpMatch(strToMatch string, globExps []string) (bool, error) {

	// Replace all backslashes with forward slashes in order for
	// glob / expression matching to work correctly with
	// the "github.com/gobwas/glob" library
	strToMatch = strings.Replace(strToMatch, "\\", "/", -1)

	for _, globExp := range globExps {

		// We replace backslashes with forward slashes for
		// glob expression / matching support
		globExp = strings.Replace(globExp, "\\", "/", -1)

		pattern, err := glob.Compile(globExp)
		if err != nil {
			return false, err
		}
		matched := pattern.Match(strToMatch)
		if matched {
			glog.V(4).Infof("ignoring path %s because of glob rule %s", strToMatch, globExp)
			return true, nil
		}
	}
	return false, nil
}

// CheckOutputFlag returns true if specified output format is supported
func CheckOutputFlag(outputFlag string) bool {
	if outputFlag == "json" || outputFlag == "" {
		return true
	}
	return false
}

// MachineOutput provides string output and error if any
// In future if we support any new format, we just need to add case in following switch case
func MachineOutput(outputFlag string, resource interface{}) (string, error) {
	var out []byte
	var err error
	switch outputFlag {
	case "json":
		// If `-o json` is provided
		out, err = json.Marshal(resource)
	}

	return string(out), err
}

// RemoveDuplicates goes through a string slice and removes all duplicates.
// Reference: https://siongui.github.io/2018/04/14/go-remove-duplicates-from-slice-or-array/
func RemoveDuplicates(s []string) []string {

	// Make a map and go through each value to see if it's a duplicate or not
	m := make(map[string]bool)
	for _, item := range s {
		if _, ok := m[item]; !ok {
			m[item] = true
		}
	}

	// Append to the unique string
	var result []string
	for item := range m {
		result = append(result, item)
	}
	return result
}
