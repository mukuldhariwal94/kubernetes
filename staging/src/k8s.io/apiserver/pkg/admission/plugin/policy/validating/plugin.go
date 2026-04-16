/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package validating

import (
	"container/list"
	"context"
	"encoding/json"
	"io"
	"sync"

	v1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/manifest/metrics"
	"k8s.io/apiserver/pkg/admission/plugin/policy/config"
	"k8s.io/apiserver/pkg/admission/plugin/policy/generic"
	"k8s.io/apiserver/pkg/admission/plugin/policy/manifest/source"
	"k8s.io/apiserver/pkg/admission/plugin/policy/matching"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
)

const (
	// PluginName indicates the name of admission plug-in
	PluginName = "ValidatingAdmissionPolicy"
)

var (
	lazyCompositionEnvTemplateWithStrictCostInit sync.Once
	lazyCompositionEnvTemplateWithStrictCost     *environment.EnvSet
)

func getCompositionEnvTemplateWithStrictCost() *environment.EnvSet {
	lazyCompositionEnvTemplateWithStrictCostInit.Do(func() {
		lazyCompositionEnvTemplateWithStrictCost = environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion())
	})
	return lazyCompositionEnvTemplateWithStrictCost
}

// Register registers a plugin
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(configFile io.Reader) (admission.Interface, error) {
		return NewPlugin(configFile)
	})
}

// Plugin is an implementation of admission.Interface.
type Policy = v1.ValidatingAdmissionPolicy
type PolicyBinding = v1.ValidatingAdmissionPolicyBinding
type PolicyEvaluator = Validator
type PolicyHook = generic.PolicyHook[*Policy, *PolicyBinding, PolicyEvaluator]

type Plugin struct {
	*generic.Plugin[PolicyHook]
	compiler *policyCompiler
}

var _ admission.Interface = &Plugin{}
var _ admission.ValidationInterface = &Plugin{}
var _ initializer.WantsExcludedAdmissionResources = &Plugin{}
var _ initializer.WantsManifestLoaders = &Plugin{}

// SetManifestLoaders provides the manifest load functions for scheme-based defaulting and validation.
func (a *Plugin) SetManifestLoaders(loaders *initializer.ManifestLoaders) {
	if loaders == nil || loaders.LoadValidatingPolicyManifests == nil {
		return
	}
	loadFunc := loaders.LoadValidatingPolicyManifests
	compiler := a.policyCompiler()
	a.SetStaticSourceFactory(func(manifestsDir string) (generic.ReloadableSource[PolicyHook], error) {
		staticSource := source.NewStaticPolicySource(manifestsDir, a.GetAPIServerID(),
			func(p *v1.ValidatingAdmissionPolicy) (Validator, error) {
				v := compiler.compile(p)
				if err := v.CompileError(); err != nil {
					return nil, err
				}
				return v, nil
			},
			func(dir string) ([]*v1.ValidatingAdmissionPolicy, []*v1.ValidatingAdmissionPolicyBinding, string, error) {
				return loadFunc(dir)
			},
			func(b *v1.ValidatingAdmissionPolicyBinding) string { return b.Spec.PolicyName },
			metrics.VAPManifestType,
		)
		if err := staticSource.LoadInitial(); err != nil {
			return nil, err
		}
		return staticSource, nil
	})
}

func NewPlugin(configFile io.Reader) (*Plugin, error) {
	cfg, err := config.LoadValidatingConfig(configFile)
	if err != nil {
		return nil, err
	}

	handler := admission.NewHandler(admission.Connect, admission.Create, admission.Delete, admission.Update)
	compiler := newPolicyCompiler()

	p := &Plugin{
		Plugin: generic.NewPlugin(
			handler,
			func(f informers.SharedInformerFactory, client kubernetes.Interface, dynamicClient dynamic.Interface, restMapper meta.RESTMapper) generic.Source[PolicyHook] {
				return generic.NewPolicySource(
					f.Admissionregistration().V1().ValidatingAdmissionPolicies().Informer(),
					f.Admissionregistration().V1().ValidatingAdmissionPolicyBindings().Informer(),
					NewValidatingAdmissionPolicyAccessor,
					NewValidatingAdmissionPolicyBindingAccessor,
					compiler.compile,
					f,
					dynamicClient,
					restMapper,
				)
			},
			func(a authorizer.Authorizer, m *matching.Matcher, client kubernetes.Interface) generic.Dispatcher[PolicyHook] {
				return NewDispatcher(a, generic.NewPolicyMatcher(m))
			},
		),
		compiler: compiler,
	}
	p.SetEnabled(true)
	p.SetStaticManifestsDir(cfg.StaticManifestsDir)
	return p, nil
}

// Validate makes an admission decision based on the request attributes.
func (a *Plugin) Validate(ctx context.Context, attr admission.Attributes, o admission.ObjectInterfaces) error {
	return a.Plugin.Dispatch(ctx, attr, o)
}

func (a *Plugin) policyCompiler() *policyCompiler {
	if a.compiler == nil {
		a.compiler = newPolicyCompiler()
	}
	return a.compiler
}

const maxPolicyCompilationCacheEntries = 10000

type policyCompiler struct {
	lock    sync.Mutex
	entries map[string]*list.Element
	order   *list.List
}

type policyCompilationCacheEntry struct {
	key      string
	compiled compiledPolicy
}

type compiledPolicy struct {
	validationFilter      cel.ConditionEvaluator
	matchConditionFilter  cel.ConditionEvaluator
	auditAnnotationFilter cel.ConditionEvaluator
	messageFilter         cel.ConditionEvaluator
	compileError          error
}

func newPolicyCompiler() *policyCompiler {
	return &policyCompiler{
		entries: map[string]*list.Element{},
		order:   list.New(),
	}
}

func (c *policyCompiler) compile(policy *Policy) Validator {
	key := policyCompilationCacheKey(policy)
	c.lock.Lock()
	if elem, ok := c.entries[key]; ok {
		c.order.MoveToFront(elem)
		compiled := elem.Value.(*policyCompilationCacheEntry).compiled
		c.lock.Unlock()
		return compiled.newValidator(policy)
	}
	c.lock.Unlock()

	compiled := compilePolicyExpressions(policy)

	c.lock.Lock()
	if elem, ok := c.entries[key]; ok {
		c.order.MoveToFront(elem)
		compiled = elem.Value.(*policyCompilationCacheEntry).compiled
	} else {
		c.entries[key] = c.order.PushFront(&policyCompilationCacheEntry{
			key:      key,
			compiled: compiled,
		})
		for c.order.Len() > maxPolicyCompilationCacheEntries {
			oldest := c.order.Back()
			entry := oldest.Value.(*policyCompilationCacheEntry)
			delete(c.entries, entry.key)
			c.order.Remove(oldest)
		}
	}
	c.lock.Unlock()

	return compiled.newValidator(policy)
}

func (c compiledPolicy) newValidator(policy *Policy) Validator {
	var matcher matchconditions.Matcher
	if c.matchConditionFilter != nil {
		matcher = matchconditions.NewMatcher(c.matchConditionFilter, policy.Spec.FailurePolicy, "policy", "validate", policy.Name)
	}
	return NewValidator(
		c.validationFilter,
		matcher,
		c.auditAnnotationFilter,
		c.messageFilter,
		policy.Spec.FailurePolicy,
		c.compileError,
	)
}

func compilePolicy(policy *Policy) Validator {
	return compilePolicyExpressions(policy).newValidator(policy)
}

func compilePolicyExpressions(policy *Policy) compiledPolicy {
	hasParam := false
	if policy.Spec.ParamKind != nil {
		hasParam = true
	}
	optionalVars := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: true}
	expressionOptionalVars := cel.OptionalVariableDeclarations{HasParams: hasParam, HasAuthorizer: false}
	var matchConditionFilter cel.ConditionEvaluator
	matchConditions := policy.Spec.MatchConditions
	compositionEnvTemplate := getCompositionEnvTemplateWithStrictCost()
	filterCompiler, err := cel.NewCompositedCompiler(compositionEnvTemplate)
	if err != nil {
		return compiledPolicy{compileError: err}
	}
	filterCompiler.CompileAndStoreVariables(convertv1beta1Variables(policy.Spec.Variables), optionalVars, environment.StoredExpressions)

	if len(matchConditions) > 0 {
		matchExpressionAccessors := make([]cel.ExpressionAccessor, len(matchConditions))
		for i := range matchConditions {
			matchExpressionAccessors[i] = (*matchconditions.MatchCondition)(&matchConditions[i])
		}
		matchConditionFilter = filterCompiler.CompileCondition(matchExpressionAccessors, optionalVars, environment.StoredExpressions)
	}
	return compiledPolicy{
		validationFilter:      filterCompiler.CompileCondition(convertv1Validations(policy.Spec.Validations), optionalVars, environment.StoredExpressions),
		matchConditionFilter:  matchConditionFilter,
		auditAnnotationFilter: filterCompiler.CompileCondition(convertv1AuditAnnotations(policy.Spec.AuditAnnotations), optionalVars, environment.StoredExpressions),
		messageFilter:         filterCompiler.CompileCondition(convertv1MessageExpressions(policy.Spec.Validations), expressionOptionalVars, environment.StoredExpressions),
	}
}

type policyCompilationKey struct {
	HasParam         bool                 `json:"hasParam"`
	Variables        []namedExpressionKey `json:"variables,omitempty"`
	MatchConditions  []namedExpressionKey `json:"matchConditions,omitempty"`
	Validations      []validationKey      `json:"validations,omitempty"`
	AuditAnnotations []auditAnnotationKey `json:"auditAnnotations,omitempty"`
}

type namedExpressionKey struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
}

type validationKey struct {
	Expression        string `json:"expression"`
	Message           string `json:"message,omitempty"`
	MessageExpression string `json:"messageExpression,omitempty"`
	Reason            string `json:"reason,omitempty"`
}

type auditAnnotationKey struct {
	Key             string `json:"key"`
	ValueExpression string `json:"valueExpression"`
}

func policyCompilationCacheKey(policy *Policy) string {
	key := policyCompilationKey{
		HasParam:         policy.Spec.ParamKind != nil,
		Variables:        make([]namedExpressionKey, 0, len(policy.Spec.Variables)),
		MatchConditions:  make([]namedExpressionKey, 0, len(policy.Spec.MatchConditions)),
		Validations:      make([]validationKey, 0, len(policy.Spec.Validations)),
		AuditAnnotations: make([]auditAnnotationKey, 0, len(policy.Spec.AuditAnnotations)),
	}
	for _, variable := range policy.Spec.Variables {
		key.Variables = append(key.Variables, namedExpressionKey{
			Name:       variable.Name,
			Expression: variable.Expression,
		})
	}
	for _, matchCondition := range policy.Spec.MatchConditions {
		key.MatchConditions = append(key.MatchConditions, namedExpressionKey{
			Name:       matchCondition.Name,
			Expression: matchCondition.Expression,
		})
	}
	for _, validation := range policy.Spec.Validations {
		reason := ""
		if validation.Reason != nil {
			reason = string(*validation.Reason)
		}
		key.Validations = append(key.Validations, validationKey{
			Expression:        validation.Expression,
			Message:           validation.Message,
			MessageExpression: validation.MessageExpression,
			Reason:            reason,
		})
	}
	for _, auditAnnotation := range policy.Spec.AuditAnnotations {
		key.AuditAnnotations = append(key.AuditAnnotations, auditAnnotationKey{
			Key:             auditAnnotation.Key,
			ValueExpression: auditAnnotation.ValueExpression,
		})
	}

	data, _ := json.Marshal(key)
	return string(data)
}

func convertv1Validations(inputValidations []v1.Validation) []cel.ExpressionAccessor {
	celExpressionAccessor := make([]cel.ExpressionAccessor, len(inputValidations))
	for i, validation := range inputValidations {
		validation := ValidationCondition{
			Expression: validation.Expression,
			Message:    validation.Message,
			Reason:     validation.Reason,
		}
		celExpressionAccessor[i] = &validation
	}
	return celExpressionAccessor
}

func convertv1MessageExpressions(inputValidations []v1.Validation) []cel.ExpressionAccessor {
	celExpressionAccessor := make([]cel.ExpressionAccessor, len(inputValidations))
	for i, validation := range inputValidations {
		if validation.MessageExpression != "" {
			condition := MessageExpressionCondition{
				MessageExpression: validation.MessageExpression,
			}
			celExpressionAccessor[i] = &condition
		}
	}
	return celExpressionAccessor
}

func convertv1AuditAnnotations(inputValidations []v1.AuditAnnotation) []cel.ExpressionAccessor {
	celExpressionAccessor := make([]cel.ExpressionAccessor, len(inputValidations))
	for i, validation := range inputValidations {
		validation := AuditAnnotationCondition{
			Key:             validation.Key,
			ValueExpression: validation.ValueExpression,
		}
		celExpressionAccessor[i] = &validation
	}
	return celExpressionAccessor
}

func convertv1beta1Variables(variables []v1.Variable) []cel.NamedExpressionAccessor {
	namedExpressions := make([]cel.NamedExpressionAccessor, len(variables))
	for i, variable := range variables {
		namedExpressions[i] = &Variable{Name: variable.Name, Expression: variable.Expression}
	}
	return namedExpressions
}
