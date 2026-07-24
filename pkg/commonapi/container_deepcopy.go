/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

package commonapi

func (in *ContainerBuildContext) DeepCopyInto(out *ContainerBuildContext) {
	*out = *in
	if in.Tags != nil {
		out.Tags = make([]string, len(in.Tags))
		copy(out.Tags, in.Tags)
	}
	if in.Args != nil {
		out.Args = make([]EnvVar, len(in.Args))
		copy(out.Args, in.Args)
	}
	if in.Secrets != nil {
		out.Secrets = make([]ContainerBuildSecret, len(in.Secrets))
		copy(out.Secrets, in.Secrets)
	}
	if in.Labels != nil {
		out.Labels = make([]Label, len(in.Labels))
		copy(out.Labels, in.Labels)
	}
}

func (in *ContainerBuildContext) DeepCopy() *ContainerBuildContext {
	if in == nil {
		return nil
	}
	out := new(ContainerBuildContext)
	in.DeepCopyInto(out)
	return out
}

func (in *ContainerNetworkConnectionConfig) DeepCopyInto(out *ContainerNetworkConnectionConfig) {
	*out = *in
	if in.Aliases != nil {
		out.Aliases = make([]string, len(in.Aliases))
		copy(out.Aliases, in.Aliases)
	}
}

func (in *ContainerNetworkConnectionConfig) DeepCopy() *ContainerNetworkConnectionConfig {
	if in == nil {
		return nil
	}
	out := new(ContainerNetworkConnectionConfig)
	in.DeepCopyInto(out)
	return out
}

func (in *FileSystemEntry) DeepCopyInto(out *FileSystemEntry) {
	*out = *in
	if in.Owner != nil {
		owner := *in.Owner
		out.Owner = &owner
	}
	if in.Group != nil {
		group := *in.Group
		out.Group = &group
	}
	if in.Entries != nil {
		out.Entries = make([]FileSystemEntry, len(in.Entries))
		for i := range in.Entries {
			in.Entries[i].DeepCopyInto(&out.Entries[i])
		}
	}
}

func (in *FileSystemEntry) DeepCopy() *FileSystemEntry {
	if in == nil {
		return nil
	}
	out := new(FileSystemEntry)
	in.DeepCopyInto(out)
	return out
}

func (in *CreateFileSystem) DeepCopyInto(out *CreateFileSystem) {
	*out = *in
	if in.Umask != nil {
		umask := *in.Umask
		out.Umask = &umask
	}
	if in.Entries != nil {
		out.Entries = make([]FileSystemEntry, len(in.Entries))
		for i := range in.Entries {
			in.Entries[i].DeepCopyInto(&out.Entries[i])
		}
	}
}

func (in *CreateFileSystem) DeepCopy() *CreateFileSystem {
	if in == nil {
		return nil
	}
	out := new(CreateFileSystem)
	in.DeepCopyInto(out)
	return out
}
