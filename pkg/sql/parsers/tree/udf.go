// Copyright 2021 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tree

type FunctionArg interface {
	NodeFormatter
}

type FunctionArgImpl struct {
	FunctionArg
}

// container holding list of arguments in udf
type FunctionArgs []FunctionArg

type FunctionArgDecl struct {
	FunctionArgImpl
	Name *UnresolvedName
	Type ResolvableTypeReference
}

func NewFunctionArgDecl(n *UnresolvedName, t ResolvableTypeReference) *FunctionArgDecl {
	return &FunctionArgDecl{
		Name: n,
		Type: t,
	}
}

func (node *FunctionArgDecl) Format(ctx *FmtCtx) {
	node.Type.(*T).InternalType.Format(ctx)
	ctx.WriteByte(' ')
	node.Name.Format(ctx)
}

type FunctionName struct {
	Name Identifier
}

type CreateFunction struct {
	statementImpl
	Name       *FunctionName
	Args       FunctionArgs
	ReturnType ResolvableTypeReference
	Body       string
	Language   string
}

func (node *FunctionName) Format(ctx *FmtCtx) {
	node.Name.Format(ctx)
}

func (node *CreateFunction) Format(ctx *FmtCtx) {
	ctx.WriteString("create ")

	ctx.WriteString("function ")

	// if node.IfNotExists {
	// 	ctx.WriteString("if not exists ")
	// }
	node.Name.Format(ctx)

	ctx.WriteString(" (")

	for i, def := range node.Args {
		if i != 0 {
			ctx.WriteString(",")
			ctx.WriteByte(' ')
		}
		def.Format(ctx)
	}

	ctx.WriteString(")")
	ctx.WriteString(" returns ")

	node.ReturnType.(*T).InternalType.Format(ctx)

	ctx.WriteString(" as '")

	ctx.WriteString(node.Body)

	ctx.WriteString("' language ")
	ctx.WriteString(node.Language)
}

func NewFuncName(name Identifier) *FunctionName {
	return &FunctionName{
		Name: name,
	}
}
