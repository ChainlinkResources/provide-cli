package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	provide "github.com/provideservices/provide-go"
	uuid "github.com/satori/go.uuid"
	"github.com/spf13/cobra"
)

var compileWorkdir string
var compileArtifactPath string
var compilerVersion string
var compilerOptimizerRuns int
var contractSourcePath string
var skipOpcodesAnalysis bool

var contractsCompileCmd = &cobra.Command{
	Use:   "compile ./path/to/project/DappTokenContract.sol --name DappTokenContract [--compiler-version 0.4.25+commit.59dbf8f1] [--analyze-opcodes]",
	Short: "Compile a smart contract from source",
	Long:  `Compile a smart contract from source, optionally targeting a specific compiler and optionally performing static analysis of assembly metadata to enable a dapp to hook into targeted opcodes observed during contract-internal transaction execution`,
	Run:   compileContract,
}

func shellOut(bash string) error {
	cmd := exec.Command("bash", "-c", bash)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	_, err := cmd.Output()
	return err
}

func makeWorkdir() (string, error) {
	_uuid, err := uuid.NewV4()
	if err != nil {
		return "", err
	}
	path := fmt.Sprintf("./.tmp-%s", _uuid)
	err = os.Mkdir(path, 0755)
	return path, err
}

func teardownWorkdir() error {
	// TODO-- add support for --debug flag, in which case the workdir is not wiped
	return os.Remove(compileWorkdir)
}

func teardownAndExit(code int) {
	defer func() {
		if r := recover(); r != nil {
			os.Exit(1)
		}
	}()
	teardownWorkdir()
	os.Exit(1)
}

func getContractABI(compiledContract map[string]interface{}) (*abi.ABI, error) {
	abiJSON, ok := compiledContract["abi"].(string)
	if !ok {
		log.Printf("Failed to retrieve contract ABI from compiled contract")
		teardownAndExit(1)
	}
	return parseContractABI([]byte(abiJSON))
}

func getContractAssembly(compiledContract map[string]interface{}) ([]interface{}, error) {
	contractAsm, ok := compiledContract["asm"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("Unable to read assembly from compiled contract: %s", compiledContract)
	}
	contractAssembly, ok := contractAsm[".code"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("Unable to read assembly from compiled contract: %s", compiledContract)
	}
	return contractAssembly, nil
}

func getContractSourceMeta(compilerOutput map[string]interface{}, contract string) (map[string]interface{}, error) {
	contractSources, ok := compilerOutput["sources"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("Unable to read contract sources from compiled contract: %s", compilerOutput)
	}
	contractSource, ok := contractSources[contract].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("Unable to read contract source for contract: %s", contract)
	}
	return contractSource, nil
}

func getContractDependencies(compilerOutput map[string]interface{}, contractPath, contract string) (map[string]interface{}, error) {
	source, err := getContractSourceMeta(compilerOutput, contractPath)
	if err != nil {
		log.Printf("Failed to retrieve contract sources from compiled contract")
		teardownAndExit(1)
	}
	ast, ok := source["AST"].(map[string]interface{})

	astExports, ok := ast["exportedSymbols"].(map[string]interface{})
	if !ok {
		log.Printf("Failed to retrieve contract exports from compiled contract AST")
		teardownAndExit(1)
	}

	reentrant := false
	if resolvedExports, ok := astExports[baseFilenameNoExt(contract)].([]interface{}); ok {
		reentrant = len(resolvedExports) > 1
	}

	exports := map[int]string{}
	for name, ids := range astExports {
		if strings.Contains(contractPath, name) {
			continue
		}

		exportIds := make([]int64, 0)
		for i := range ids.([]interface{}) {
			exportIds = append(exportIds, int64(ids.([]interface{})[i].(float64)))
		}
		exports[int(exportIds[0])] = name
	}

	nodes, ok := ast["nodes"].([]interface{})
	if !ok {
		log.Printf("Failed to retrieve contract nodes from compiled contract AST")
		teardownAndExit(1)
	}
	if len(nodes) <= 1 {
		log.Printf("Failed to retrieve contract dependencies from compiled contract nodes; malformed AST?")
		teardownAndExit(1)
	}

	dependencies := map[string]interface{}{}

	for i := range exports {
		dependencyContractKey := exports[i]
		dependencyContractKeyParts := strings.Split(dependencyContractKey, ":")
		dependencyContractName := dependencyContractKeyParts[len(dependencyContractKeyParts)-1]
		dependencyContractPath := strings.Replace(contractPath, dependencyContractName, dependencyContractKey, -1)
		dependencyContractSourceMetaKey := strings.Replace(contractPath, dependencyContractName, dependencyContractKey, -1)

		_dependencyContractKey := fmt.Sprintf("%s:%s", dependencyContractPath, baseFilenameNoExt(dependencyContractKey))

		dependencyContract := compilerOutput["contracts"].(map[string]interface{})[_dependencyContractKey].(map[string]interface{})
		dependencyContractABI, _ := getContractABI(dependencyContract)
		dependencyContractBytecode, _ := getContractABI(dependencyContract) // TODO: remove this it is not needed...
		dependencyContractAssembly, _ := getContractAssembly(dependencyContract)

		var deps map[string]interface{}

		if reentrant {
			deps, _ = getContractDependencies(compilerOutput, dependencyContractPath, dependencyContractSourceMetaKey)
		}

		dependencies[dependencyContractName] = map[string]interface{}{
			"name":     dependencyContractName,
			"abi":      dependencyContractABI,
			"bytecode": dependencyContractBytecode,
			"assembly": dependencyContractAssembly,
			"deps":     deps,
			"raw":      dependencyContract,
		}
	}

	return dependencies, nil
}

func getContractBytecode(compiledContract map[string]interface{}) ([]byte, error) {
	bytecode, ok := compiledContract["bin"].(string)
	if !ok {
		return nil, fmt.Errorf("Unable to read bytecode from compiled contract: %s", compiledContract)
	}
	return []byte(bytecode), nil
}

func parseCachedArtifact() (map[string]interface{}, error) {
	artifactJSON, err := ioutil.ReadFile(compileArtifactPath)
	if err != nil {
		log.Printf("Failed to read compiled artifact JSON; %s", err.Error())
		teardownAndExit(1)
	}

	artifact := map[string]interface{}{}
	err = json.Unmarshal(artifactJSON, &artifact)
	return artifact, err
}

func parseContractABI(contractABIJSON []byte) (*abi.ABI, error) {
	abival, err := abi.JSON(strings.NewReader(string(contractABIJSON)))
	if err != nil {
		log.Printf("Failed to initialize ABI from contract params to json; %s", err.Error())
		teardownAndExit(1)
	}

	return &abival, nil
}

func parseCompilerOutput(path string) (compiledContracts map[string]interface{}, err error) {
	compilerOutputJSON, err := ioutil.ReadFile(path)
	if err != nil {
		log.Printf("Failed to read compiled, combined contract JSON; %s", err.Error())
		teardownAndExit(1)
	}

	combinedOutput := map[string]interface{}{}
	err = json.Unmarshal(compilerOutputJSON, &combinedOutput)
	return combinedOutput, err
}

func parseCompiledContracts(path string) (compiledContracts map[string]interface{}, err error) {
	combinedOutput, err := parseCompilerOutput(path)
	if err == nil {
		compiledContracts = combinedOutput["contracts"].(map[string]interface{})
		return compiledContracts, err
	}
	return nil, err
}

func baseFilenameNoExt(path string) string {
	filename := filepath.Base(path)
	extension := filepath.Ext(filename)
	return strings.TrimRight(filename, extension)
}

func buildCompileCommand(sourcePath string, optimizerRuns int) string {
	return fmt.Sprintf("solc --optimize --optimize-runs %d --pretty-json --combined-json abi,asm,ast,bin,bin-runtime,clone-bin,compact-format,devdoc,hashes,interface,metadata,opcodes,srcmap,srcmap-runtime,userdoc -o %s %s", optimizerRuns, compileWorkdir, sourcePath)
	// TODO: run optimizer over certain sources if identified for frequent use via contract-internal CREATE opcodes?
}

func compile(sourcePath string) {
	name := baseFilenameNoExt(sourcePath)
	log.Printf("Resolved contract base name: %s", name)

	compiledContractPath := fmt.Sprintf("%s/combined.json", compileWorkdir)
	log.Printf("Attempting to compile contract(s) %s from source: %s; target: %s", name, sourcePath, compiledContractPath)

	err := shellOut(buildCompileCommand(sourcePath, compilerOptimizerRuns))
	if err != nil {
		log.Printf("Failed to compile contract(s): %s; %s", name, err.Error())
		teardownAndExit(1)
	}

	if _, err := os.Stat(compiledContractPath); err != nil {
		log.Printf("Failed to compile contract(s): %s; %s", name, err.Error())
		teardownAndExit(1)
	}

	compilerOutput, err := parseCompilerOutput(compiledContractPath)
	contracts, err := parseCompiledContracts(compiledContractPath)
	if err != nil {
		log.Printf("Failed to compile contract(s): %s; %s", name, err.Error())
		teardownAndExit(1)
	}

	depGraph := map[string]interface{}{}
	var topLevelConstructor *abi.Method

	for key := range contracts {
		keyParts := strings.Split(key, ":")
		contractName := keyParts[len(keyParts)-1]
		contract := contracts[key].(map[string]interface{})

		_abi, _ := getContractABI(contract)
		bytecode, _ := getContractBytecode(contract)
		assembly, _ := getContractAssembly(contract)

		contractSourceMetaKey := strings.Replace(sourcePath, name, contractName, -1)
		contractDependencies, err := getContractDependencies(compilerOutput, sourcePath, contractSourceMetaKey)
		if err != nil {
			log.Printf("WARNING: failed to retrieve contract dependencies for contract: %s", contractName)
			teardownAndExit(1)
		}

		depGraph[contractName] = map[string]interface{}{
			"name":     contractName,
			"abi":      _abi,
			"bytecode": bytecode,
			"assembly": assembly,
			"deps":     contractDependencies,
			"raw":      contract,
		}

		if name == contractName {
			topLevelConstructor = &_abi.Constructor
		}
	}

	if topLevelConstructor == nil {
		log.Printf("WARNING: no top-level contract resolved for %s", name)
		teardownAndExit(1)
	}

	var artifact map[string]interface{} // this is the artifact compatible with the provide-cli contract deployment and will be cached on disk temporarily

	var invocationSig string
	for name, meta := range depGraph {
		if strings.Contains(sourcePath, name) {
			bytecode := meta.(map[string]interface{})["bytecode"].([]byte)
			invocationSig = fmt.Sprintf("0x%s", string(bytecode))

			artifact = meta.(map[string]interface{})

			// graph := meta.(map[string]interface{})["deps"].(map[string]interface{})
			log.Printf("TODO: match supported opcodes for %s", name)
		}
	}

	argvLength := topLevelConstructor.Inputs.LengthNonIndexed()
	constructorParams := make([]interface{}, argvLength)
	if argvLength > 0 {
		for i := range topLevelConstructor.Inputs {
			input := topLevelConstructor.Inputs[i]
			val := requireConstructorParamValue(input.Name)
			constructorParams[i] = val
		}
	}

	if len(constructorParams) != argvLength {
		log.Printf("Constructor for %s contract requires %d parameters at compile-time; given: %d", name, argvLength, len(constructorParams))
		teardownAndExit(1)
	}

	encodedArgv, err := provide.EncodeABI(topLevelConstructor, constructorParams...)
	if err != nil {
		log.Printf("WARNING: failed to encode %d parameters prior to compiling contract: %s; %s", len(constructorParams), name, err.Error())
		teardownAndExit(1)
	}

	invocationSig = fmt.Sprintf("%s%s", invocationSig, common.ToHex(encodedArgv)[8:])
	artifact["bytecode"] = invocationSig

	artifactJSON, err := json.Marshal(artifact)
	if err != nil {
		log.Printf("WARNING: failed to marshal compiled artifact for contract: %s; %s", name, err.Error())
		teardownAndExit(1)
	}

	ioutil.WriteFile(compileArtifactPath, artifactJSON, 0644)
}

func requireConstructorParamValue(name string) string {
	fmt.Printf("%s: ", name)
	reader := bufio.NewReader(os.Stdin)
	val, err := reader.ReadString('\n')
	if err != nil {
		log.Println(err)
		teardownAndExit(1)
	}
	val = strings.Trim(val, "\n")
	if val == "" {
		log.Printf("Constructor parameter %s required to compile deployable contract bytecode", name)
		teardownAndExit(1)
	}
	return val
}

// compileContract compiles a smart contract or truffle project from source.
func compileContract(cmd *cobra.Command, args []string) {
	contractSourcePath = args[0]
	source, err := os.Stat(contractSourcePath)
	if os.IsNotExist(err) {
		log.Printf("Contract source does not exist at %s; %s", compileWorkdir, err.Error())
		teardownAndExit(1)
	}
	switch mode := source.Mode(); {
	case mode.IsDir():
		log.Printf("Recursive contract source compilation not yet supported; compile path requested: %s", compileWorkdir)
		teardownAndExit(1)
	case mode.IsRegular():
		// no-op
	}

	if compileWorkdir == "" {
		compileWorkdir, err = makeWorkdir()
	}
	target, err := os.Stat(compileWorkdir)
	if os.IsNotExist(err) {
		log.Printf("Creating temporary contract working directory for compiling source at %s; %s", contractSourcePath, err.Error())
	}
	switch mode := target.Mode(); {
	case mode.IsDir():
		// no-op
		// TODO: clean workdir?
	case mode.IsRegular():
		// no-op
		log.Printf("Contract source compilation attempted to target existing file; path requested: %s", compileWorkdir)
		teardownAndExit(1)
	}

	if compileArtifactPath == "" {
		compileArtifactPath = fmt.Sprintf("%s/provide-artifact.json", compileWorkdir)
	}

	compile(contractSourcePath)
}

func init() {
	contractsCompileCmd.Flags().StringVar(&compilerVersion, "compiler-version", "latest", "target compiler version")
	contractsCompileCmd.Flags().StringVar(&compileWorkdir, "workdir", "", "path to temporary working directory for compiled artifacts")
	contractsCompileCmd.Flags().BoolVar(&skipOpcodesAnalysis, "skip-opcodes-analysis", false, "when true, static analysis of assembly for contract-internal ABI metadata is skipped")
	contractsCompileCmd.Flags().IntVar(&compilerOptimizerRuns, "optimizer-runs", 200, "set the number of runs to optimize for in terms of initial deployment cost; higher values optimize more for high-frequency usage; may not be supported by all compilers")
}