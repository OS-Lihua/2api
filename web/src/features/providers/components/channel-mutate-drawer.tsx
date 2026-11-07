/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import {
  SideDrawerSection,
  sideDrawerContentClassName,
  sideDrawerFooterClassName,
  sideDrawerFormClassName,
  sideDrawerHeaderClassName,
} from '@/components/drawer-layout'
import { JsonEditor } from '@/components/json-editor'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { useDebounce } from '@/hooks/use-debounce'
import { api } from '@/lib/api'
import {
  getServerErrorMessage,
  requireServerSuccess,
} from '@/lib/server-error-message'

import {
  findChannelModels,
  saveSingleModelChannel,
  suggestChannelModels,
  type SingleModelChannel,
} from '../channel-api'
import { channelFormSchema, type ChannelFormValues } from '../lib/channel-form'

export function ChannelMutateDrawer(props: {
  providerId: number
  channel?: SingleModelChannel
  onClose: () => void
  onSaved: () => void
}) {
  const { t } = useTranslation()
  const [keyword, setKeyword] = useState('')
  const filter = useDebounce(keyword, 300)
  const form = useForm<ChannelFormValues>({
    resolver: zodResolver(channelFormSchema),
    defaultValues: {
      upstream_model_name: props.channel?.upstream_model_name ?? '',
      protocol: props.channel?.protocol ?? '',
      model_id: props.channel ? String(props.channel.model_id) : '',
      rpm: String(props.channel?.rpm ?? 0),
      tpm: String(props.channel?.tpm ?? 0),
      max_concurrency: String(props.channel?.max_concurrency ?? 0),
      weight: String(props.channel?.weight ?? 1),
      capabilities: props.channel?.capabilities
        ? JSON.stringify(props.channel.capabilities, null, 2)
        : '',
      parameter_overrides: JSON.stringify(
        props.channel?.parameter_overrides ?? {},
        null,
        2
      ),
    },
  })
  const models = useQuery({
    queryKey: ['channel-model-options', filter],
    queryFn: () => findChannelModels(filter),
  })
  const protocols = useQuery({
    queryKey: ['channel-protocols'],
    queryFn: async () => {
      const response = await api.get('/api/admin/v1/channels/protocols')
      return requireServerSuccess<{ success: boolean; data: string[] }>(
        response.data
      ).data
    },
  })
  const suggest = useMutation({
    mutationFn: () =>
      suggestChannelModels(form.getValues('upstream_model_name')),
    onSuccess: (result) => {
      if (result.selected_model_id !== null) {
        form.setValue('model_id', String(result.selected_model_id), {
          shouldDirty: true,
        })
      }
    },
  })
  const save = useMutation({
    meta: { errorToast: false },
    mutationFn: async (values: ChannelFormValues) => {
      await saveSingleModelChannel(props.channel?.id, {
        provider_id: props.providerId,
        model_id: Number(values.model_id),
        protocol: values.protocol,
        upstream_model_name: values.upstream_model_name,
        rpm: Number(values.rpm),
        tpm: Number(values.tpm),
        max_concurrency: Number(values.max_concurrency),
        weight: Number(values.weight),
        parameter_overrides: JSON.parse(values.parameter_overrides),
        capabilities:
          values.capabilities.trim() === ''
            ? null
            : JSON.parse(values.capabilities),
      })
    },
    onSuccess: props.onSaved,
    onError: (error) =>
      form.setError('root.server', {
        message: getServerErrorMessage(error, t('Operation failed')),
      }),
  })
  const modelOptions = (models.data?.items ?? []).map((model) => ({
    value: String(model.id),
    label: model.name,
  }))
  const selected = form.watch('model_id')
  if (selected && !modelOptions.some((option) => option.value === selected)) {
    modelOptions.push({
      value: selected,
      label: `${t('Model ID')} ${selected}`,
    })
  }
  const protocolOptions = (protocols.data ?? []).map((value) => ({
    value,
    label: value,
  }))
  return (
    <Sheet
      open
      onOpenChange={(open) => {
        if (!open && !save.isPending) props.onClose()
      }}
    >
      <SheetContent className={sideDrawerContentClassName('sm:max-w-[600px]')}>
        <SheetHeader className={sideDrawerHeaderClassName()}>
          <SheetTitle>
            {props.channel ? t('Edit Channel') : t('New Channel')}
          </SheetTitle>
          <SheetDescription>
            {t('One provider, one protocol, and one upstream model.')}
          </SheetDescription>
        </SheetHeader>
        <Form {...form}>
          <form
            id='single-channel-form'
            className={sideDrawerFormClassName()}
            onSubmit={form.handleSubmit((values) => save.mutate(values))}
          >
            <fieldset disabled={save.isPending} className='contents'>
              <SideDrawerSection>
                <FormField
                  control={form.control}
                  name='upstream_model_name'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Upstream model name')}</FormLabel>
                      <FormControl>
                        <Input {...field} />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <Button
                  type='button'
                  variant='outline'
                  disabled={
                    suggest.isPending || !form.watch('upstream_model_name')
                  }
                  onClick={() => suggest.mutate()}
                >
                  {t('Suggest Model')}
                </Button>
                {suggest.isSuccess &&
                  suggest.data.selected_model_id === null && (
                    <p role='status'>
                      {t('Choose a Model: no unique match was found.')}
                    </p>
                  )}
                <FormField
                  control={form.control}
                  name='protocol'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Protocol')}</FormLabel>
                      <Select
                        items={protocolOptions}
                        value={field.value}
                        onValueChange={field.onChange}
                      >
                        <FormControl>
                          <SelectTrigger className='w-full'>
                            <SelectValue />
                          </SelectTrigger>
                        </FormControl>
                        <SelectContent>
                          {protocolOptions.map((option) => (
                            <SelectItem key={option.value} value={option.value}>
                              {option.label}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <label className='space-y-2 text-sm'>
                  {t('Filter by model name...')}
                  <Input
                    value={keyword}
                    onChange={(event) => setKeyword(event.target.value)}
                  />
                </label>
                <FormField
                  control={form.control}
                  name='model_id'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Model')}</FormLabel>
                      <Select
                        items={modelOptions}
                        value={field.value}
                        onValueChange={field.onChange}
                      >
                        <FormControl>
                          <SelectTrigger className='w-full'>
                            <SelectValue />
                          </SelectTrigger>
                        </FormControl>
                        <SelectContent>
                          {modelOptions.map((option) => (
                            <SelectItem key={option.value} value={option.value}>
                              {option.label}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              </SideDrawerSection>
              <SideDrawerSection>
                <div className='grid gap-4 sm:grid-cols-2'>
                  {(['rpm', 'tpm', 'max_concurrency', 'weight'] as const).map(
                    (name) => (
                      <FormField
                        key={name}
                        control={form.control}
                        name={name}
                        render={({ field }) => (
                          <FormItem>
                            <FormLabel>
                              {t(
                                {
                                  rpm: 'RPM',
                                  tpm: 'TPM',
                                  max_concurrency: 'Max concurrency',
                                  weight: 'Weight',
                                }[name]
                              )}
                            </FormLabel>
                            <FormControl>
                              <Input inputMode='numeric' {...field} />
                            </FormControl>
                            <FormMessage />
                          </FormItem>
                        )}
                      />
                    )
                  )}
                </div>
              </SideDrawerSection>
              <SideDrawerSection>
                <p className='text-muted-foreground text-sm'>
                  {t(
                    'Leave capabilities empty to inherit the Model. A declaration can only narrow it; omitted limits inherit the Model limits.'
                  )}
                </p>
                {(['capabilities', 'parameter_overrides'] as const).map(
                  (name) => (
                    <FormField
                      key={name}
                      control={form.control}
                      name={name}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>
                            {name === 'capabilities'
                              ? t('Capabilities')
                              : t('Parameter overrides')}
                          </FormLabel>
                          <JsonEditor
                            value={field.value}
                            onChange={field.onChange}
                            disabled={save.isPending}
                            valueType='any'
                            template={
                              name === 'capabilities'
                                ? {
                                    features: [],
                                    input_modalities: [],
                                    output_modalities: [],
                                    limits: {},
                                  }
                                : {}
                            }
                          />
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                  )
                )}
                {form.formState.errors.root?.server?.message && (
                  <p role='alert' className='text-destructive text-sm'>
                    {form.formState.errors.root.server.message}
                  </p>
                )}
              </SideDrawerSection>
            </fieldset>
          </form>
        </Form>
        <SheetFooter className={sideDrawerFooterClassName()}>
          <Button
            variant='outline'
            onClick={props.onClose}
            disabled={save.isPending}
          >
            {t('Close')}
          </Button>
          <Button
            type='submit'
            form='single-channel-form'
            disabled={save.isPending}
          >
            {save.isPending ? t('Saving...') : t('Save changes')}
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  )
}
